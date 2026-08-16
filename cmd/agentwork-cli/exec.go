package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/proto/acpbackend"
	"github.com/eushing/agentwork/internal/runtime"
)

// executor is the machine's run engine (CLI 分支 Phase 2): run.dispatch
// spawns the ACP runtime with the daemon-assembled prompt, streams events
// back (per-run seq, 100ms batches), and reports the terminal state via
// run.finished. The machine NEVER queries the platform — the dispatch
// payload is a complete readonly snapshot.
type executor struct {
	peer *link.Peer
	// serverURL is the platform address THIS machine dialed (connect
	// --server / default) — the same address the agent's CLI callbacks use.
	serverURL string

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // runID → turn cancel
}

func newExecutor(peer *link.Peer, serverURL string) *executor {
	return &executor{peer: peer, serverURL: serverURL, cancels: map[string]context.CancelFunc{}}
}

// shutdown cancels every in-flight run (the link died — the platform will
// reclaim them via RecoverStuckRunning).
func (e *executor) shutdown() {
	e.mu.Lock()
	n := len(e.cancels)
	for _, cancel := range e.cancels {
		cancel()
	}
	e.cancels = map[string]context.CancelFunc{}
	e.mu.Unlock()
	if n > 0 {
		cliLogf("exec: link died — cancelling %d in-flight run(s)", n)
	}
}

// handleDispatch implements run.dispatch on the machine link.
func (e *executor) handleDispatch(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
	var p link.RunDispatchParams
	if err := json.Unmarshal(raw, &p); err != nil || p.RunID == "" || p.Prompt == "" || len(p.ACPSpawn) == 0 {
		return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "run_id, prompt, and acp_spawn are required"}
	}
	cliLogf("exec: run %s dispatch received (role=%s attempt=%d agent=%s proc=%v)", p.RunID, p.Role, p.Attempt, p.AgentID, p.Proc)
	go e.execute(p)
	return link.RunDispatchResult{Accepted: true}, nil
}

// handleCancel implements run.cancel (notification).
func (e *executor) handleCancel(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
	var p link.RunCancelParams
	_ = json.Unmarshal(raw, &p)
	cliLogf("exec: run %s cancel received (reason=%s)", p.RunID, p.Reason)
	e.mu.Lock()
	cancel := e.cancels[p.RunID]
	delete(e.cancels, p.RunID)
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil, nil
}

// execute runs one dispatched run to completion.
func (e *executor) execute(p link.RunDispatchParams) {
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancels[p.RunID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, p.RunID)
		e.mu.Unlock()
		cancel()
	}()

	// Workdir: processor runs get the proc dir (compile-on-repo = a
	// detached worktree of origin/<default>); scratch domains derive the
	// local directory layout (mirrors the daemon's); git domains get a
	// worktree on the goal branch — and the completed run's changes are
	// committed + pushed as agentwork/<branch> (中转, Phase 3).
	var workdir, repo, branch, sessionID string
	var cleanupGit func()
	if p.Proc {
		workdir = machineProcWorkdir(p.RunID)
		if p.Scratch {
			if err := os.MkdirAll(workdir, 0o755); err != nil {
				e.finish(p, "", "", "failed", fmt.Sprintf("workdir: %v", err))
				return
			}
		} else {
			var err error
			repo, err = ensureBareRepo(ctx, p)
			if err != nil {
				e.finish(p, "", "", "failed", fmt.Sprintf("git: %v", err))
				return
			}
			def := p.DefaultBranch
			if def == "" {
				def = "main"
			}
			if out, err := runGit(ctx, repo, "worktree", "add", "--force", "--detach", workdir, "origin/"+def); err != nil {
				e.finish(p, "", "", "failed", fmt.Sprintf("worktree add: %v: %s", err, out))
				return
			}
			defer func() {
				_, _ = runGit(context.Background(), repo, "worktree", "remove", "--force", workdir)
			}()
		}
		// The artifact's ABSOLUTE paths — the proc dir is opaque to the
		// agent (it once guessed a path and the artifact never landed).
		if len(p.ArtifactFiles) > 0 {
			p.Prompt += fmt.Sprintf("\n\nThe artifact files' ABSOLUTE directory: %s\n(Write %s there with your file tools; do NOT guess the working directory.)\n",
				workdir, strings.Join(p.ArtifactFiles, ", "))
		}
	} else if p.Scratch {
		var err error
		workdir, err = scratchWorkdir(p)
		if err != nil {
			e.finish(p, "", "", "failed", fmt.Sprintf("workdir: %v", err))
			return
		}
	} else {
		var err error
		workdir, repo, branch, cleanupGit, err = ensureGitWorkdir(ctx, p)
		if err != nil {
			e.finish(p, "", "", "failed", fmt.Sprintf("git: %v", err))
			return
		}
		// The cleanup runs AFTER the finish report — a hang here (repo lock
		// contention with the daemon's completion worktrees) stalls the
		// sidecar's hottest window. Probe it: the timeout keeps the process
		// responsive and the log line tells us where the stall was.
		defer func() {
			done := make(chan struct{})
			go func() { cleanupGit(); close(done) }()
			select {
			case <-done:
				cliLogf("exec: run %s git cleanup done", p.RunID)
			case <-time.After(60 * time.Second):
				cliLogf("exec: run %s git cleanup TIMED OUT after 60s (worktree left for the daemon's sweeps)", p.RunID)
			}
		}()
	}

	// The agent's persona (pushed via config.push) rides AGENTS.md in the
	// workdir — the runtime loads it natively. The run's CONTEXT layer
	// (platform background, goal, team, role contract, tool surface —
	// shipped in the dispatch) is persona material too and is merged into
	// the same file; the prompt carries the task only.
	pushedProfile := ""
	if !p.Proc {
		pushedProfile = syncAgentProfile(p.AgentID, workdir)
		if p.RunProfile != "" {
			if b, err := os.ReadFile(filepath.Join(workdir, "AGENTS.md")); err == nil {
				pushedProfile = string(b) + "\n\n" + p.RunProfile
				_ = os.WriteFile(filepath.Join(workdir, "AGENTS.md"), []byte(pushedProfile), 0o644)
			}
		}
		// Skills ride the workdir too (project-level): staged from the
		// pushed dir under their ORIGINAL names — the user's own global
		// skills are never touched. Scratch workdirs are platform-owned
		// and wiped first so deselected skills disappear; repo workdirs
		// are overwrite-only (the repo may have its own files there).
		// The dir comes from the machine's probe report (dispatch);
		// '' = legacy daemon — fall back to the local CLI mapping.
		subdir := p.ProjectSkillsDir
		if subdir == "" {
			subdir = skillSubdir(p.ACPSpawn[0])
		}
		copyAgentSkills(p.AgentID, workdir, subdir, p.Scratch)
	}

	conn, err := runtime.Open(ctx, runtime.Spec{
		Transport:  "stdio",
		Executable: p.ACPSpawn[0],
		Args:       p.ACPSpawn[1:],
		Env:        p.Env,
		Cwd:        workdir,
	}, e.buildEnv(p, workdir))
	if err != nil {
		e.finish(p, "", "", "failed", fmt.Sprintf("runtime: %v", err))
		return
	}

	backend := &acpbackend.Backend{}
	sess, err := backend.OpenSession(ctx, proto.SessionSpec{Conn: conn, Cwd: workdir})
	if err != nil {
		_ = conn.Close()
		e.finish(p, "", "", "failed", fmt.Sprintf("session: %v", err))
		return
	}
	sessionID = sess.SessionID()

	// Session resume (multica-style): a prior session id resolves ONLY in
	// the exact workdir it was recorded against — agent CLI session stores
	// are keyed by cwd. Mismatch: drop the pointer and say so in the
	// prompt (never silently restart). Load failure: the CLI's fresh
	// process cannot resolve the id — continue on the fresh session; the
	// cwd-keyed project store still gives partial continuity.
	if p.PriorSessionID != "" && !p.Proc {
		if p.PriorWorkDir == workdir {
			if err := sess.LoadSession(ctx, p.PriorSessionID); err != nil {
				cliLogf("exec: run %s session/load %s failed: %v — continuing fresh", p.RunID, shortSHA(p.PriorSessionID), err)
			} else {
				cliLogf("exec: run %s session resumed (%s)", p.RunID, shortSHA(p.PriorSessionID))
			}
		} else {
			p.Prompt += "\n\nYour previous session could not be resumed: the working directory for this turn differs from the recorded one — you are starting fresh in this workspace."
			cliLogf("exec: run %s prior session dropped (workdir %s != prior %s)", p.RunID, workdir, p.PriorWorkDir)
		}
	}

	// Probe like the git cleanup: a session close that hangs (child process
	// refusing to die) must not stall the sidecar.
	defer func() {
		done := make(chan struct{})
		go func() { sess.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			cliLogf("exec: run %s session close TIMED OUT after 30s", p.RunID)
		}
	}()

	// Ack the daemon: execution really started (run.claimed — the review
	// window's "审查中" flip hangs on it).
	if err := callRPC(e.peer, link.MethodRunClaimed, link.RunClaimedParams{RunID: p.RunID}, nil, 15*time.Second); err != nil {
		cliLogf("exec: run %s claimed ack failed: %v", p.RunID, err)
	}

	run, err := sess.Prompt(ctx, p.Prompt)
	if err != nil {
		e.finish(p, workdir, sessionID, "failed", fmt.Sprintf("prompt: %v", err))
		return
	}

	// Event pump: per-run monotonic seq, batched every ~100ms (or 64
	// events) — the per-token stream must not become per-token frames.
	seq := int64(0)
	buf := []link.RunEvent{}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	flushErrLogged := false
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := e.peer.Notify(ctx, link.MethodRunEventBatch, link.RunEventBatchParams{
			RunID: p.RunID, SeqStart: buf[0].Seq, Events: buf,
		}); err != nil && !flushErrLogged {
			// First failure only — a dead link would otherwise flood the
			// log every 100ms.
			cliLogf("exec: run %s event batch failed (seq %d..%d, %d events): %v", p.RunID, buf[0].Seq, buf[len(buf)-1].Seq, len(buf), err)
			flushErrLogged = true
		}
		buf = nil
	}
	defer flush()
loop:
	for {
		select {
		case ev, ok := <-run.Events:
			if !ok {
				flush()
				break loop
			}
			seq++
			buf = append(buf, link.RunEvent{
				Seq: seq, Kind: string(ev.Type), Text: ev.Text,
				Tool: ev.Tool, Input: ev.Input, Output: ev.Output,
			})
			if len(buf) >= 64 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			_ = sess.Cancel(context.Background())
			e.finish(p, workdir, sessionID, "cancelled", "cancelled by platform")
			return
		}
	}

	result, ok := <-run.Result
	if !ok {
		e.finish(p, workdir, sessionID, "failed", "backend closed result channel")
		return
	}
	status := "completed"
	switch result.Status {
	case proto.StatusFailed, proto.StatusAborted:
		status = "failed"
	case proto.StatusCancelled:
		status = "cancelled"
	}
	headSHA := ""
	if status == "completed" && !p.Scratch && !p.Proc {
		sha, err := commitAndPush(context.Background(), p, workdir, repo, branch, p.RunID, pushedProfile)
		if err != nil {
			cliLogf("exec: run %s commit/push failed: %v", p.RunID, err)
			e.finish(p, workdir, sessionID, "failed", "commit/push: "+err.Error())
			return
		}
		if sha != "" {
			cliLogf("exec: run %s pushed %s → agentwork/%s", p.RunID, shortSHA(sha), branch)
		}
		headSHA = sha
	}
	// Processor runs upload their FILE results (the platform reads
	// structured side effects, never agent stdout).
	finishParams := link.RunFinishedParams{RunID: p.RunID, Status: status, Summary: result.Output, Token: p.Token, HeadSHA: headSHA, SessionID: sessionID, WorkDir: workdir}
	if p.Proc && status == "completed" {
		finishParams.Artifacts = map[string]string{}
		for _, f := range p.ArtifactFiles {
			if b, err := os.ReadFile(filepath.Join(workdir, f)); err == nil {
				finishParams.Artifacts[f] = string(b)
			}
		}
	}
	if err := callRPC(e.peer, link.MethodRunFinished, finishParams, nil, 30*time.Second); err != nil {
		// A lost terminal report would leave the run 'running' until the
		// daemon restarts — stash it for the next register.
		cliLogf("exec: run %s → %s (report stashed — link down: %v)", p.RunID, status, err)
		stashPendingReport(finishParams)
		return
	}
	cliLogf("exec: run %s → %s", p.RunID, status)
}

// finish uploads run.finished — the daemon runs Finish + reconcile. If the
// link is down at this exact moment, the report is STASHED to disk and
// flushed on the next successful register (a lost terminal report would
// leave the run hanging 'running' until the daemon restarts). The report
// echoes the dispatch's per-run TOKEN: the daemon swallows stale reports
// (an exec killed before a daemon restart must not cancel the attempt the
// daemon re-dispatched after recovery).
func (e *executor) finish(p link.RunDispatchParams, workdir, sessionID, status, summary string) {
	report := link.RunFinishedParams{RunID: p.RunID, Status: status, Summary: summary, Token: p.Token, SessionID: sessionID, WorkDir: workdir}
	err := callRPC(e.peer, link.MethodRunFinished, report, nil, 30*time.Second)
	if err == nil {
		cliLogf("exec: run %s → %s", p.RunID, status)
		return
	}
	cliLogf("exec: run %s → %s (report stashed — link down: %v)", p.RunID, status, err)
	stashPendingReport(report)
}

// shortSHA trims a commit sha for log lines.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// pendingReportFile is where unreported terminal states wait for the next
// connection.
func pendingReportFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pending-reports.json"
	}
	return filepath.Join(home, ".agentwork", "pending-reports.json")
}

// stashPendingReport appends a terminal report that could not be delivered.
func stashPendingReport(p link.RunFinishedParams) {
	reports, _ := loadPendingReports()
	reports = append(reports, p)
	savePendingReports(reports)
}

func loadPendingReports() ([]link.RunFinishedParams, error) {
	b, err := os.ReadFile(pendingReportFile())
	if err != nil {
		return nil, err
	}
	var out []link.RunFinishedParams
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func savePendingReports(reports []link.RunFinishedParams) {
	_ = os.MkdirAll(filepath.Dir(pendingReportFile()), 0o755)
	if len(reports) == 0 {
		_ = os.Remove(pendingReportFile())
		return
	}
	b, _ := json.MarshalIndent(reports, "", "  ")
	_ = os.WriteFile(pendingReportFile(), b, 0o644)
}

// flushPendingReports re-sends stashed terminal reports over a fresh link
// (called after a successful register). Leftovers stay stashed.
func flushPendingReports(peer *link.Peer) {
	reports, err := loadPendingReports()
	if err != nil || len(reports) == 0 {
		return
	}
	var remaining []link.RunFinishedParams
	for _, r := range reports {
		if err := callRPC(peer, link.MethodRunFinished, r, nil, 30*time.Second); err != nil {
			remaining = append(remaining, r)
		} else {
			fmt.Printf("flushed pending report for run %s (%s)\n", r.RunID, r.Status)
		}
	}
	savePendingReports(remaining)
}

// agentProfileDir is where config.push lands the agent's AGENTS.md
// (~/.agentwork/agents/<agentID>/).
func agentProfileDir(agentID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "agents", agentID)
}

// writeAgentProfile persists the pushed system prompt as AGENTS.md
// ('' removes the file — an empty prompt leaves no profile).
func writeAgentProfile(agentID, systemPrompt string) error {
	dir := agentProfileDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "AGENTS.md")
	if strings.TrimSpace(systemPrompt) == "" {
		_ = os.Remove(path)
		return nil
	}
	return os.WriteFile(path, []byte(systemPrompt), 0o644)
}

// syncAgentProfile copies the agent's pushed AGENTS.md into the run's
// workdir (the runtime's profile resolver walks up from cwd). Returns the
// profile's content ('' = none pushed) so the commit step can recognize it.
func syncAgentProfile(agentID, workdir string) string {
	b, err := os.ReadFile(filepath.Join(agentProfileDir(agentID), "AGENTS.md"))
	if err != nil {
		return ""
	}
	content := string(b)
	_ = os.WriteFile(filepath.Join(workdir, "AGENTS.md"), b, 0o644)
	return content
}

// machineSkillRoot is where config.push lands an agent's skill packages on
// the machine (~/.agentwork/skills/<agentID>/<skillName>/...) — the
// platform-managed staging dir; the executor copies them into each run's
// workdir at spawn.
func machineSkillRoot(agentID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "skills", agentID)
}

// skillSubdir maps a probed CLI (the acp_spawn executable) to its
// project-level skill directory inside a run workdir — the legacy-daemon
// fallback when the dispatch carries no ProjectSkillsDir. Project skills
// load from cwd — no global install, no collision with the user's own
// skills.
func skillSubdir(cli string) string {
	base := filepath.Base(cli)
	switch {
	case strings.HasPrefix(base, "opencode"):
		return ".opencode/skill"
	case strings.HasPrefix(base, "claude"):
		return ".claude/skills"
	default:
		return ".agents/skills" // the AgentSkills standard (npx skills add)
	}
}

// copyAgentSkills stages the agent's pushed skill packages into the
// workdir's project-level skill directory. wipe removes the target subdir
// first (platform-owned scratch workdirs); repo workdirs are overwrite-only
// — the repository's own files in that path are preserved.
func copyAgentSkills(agentID, workdir, subdir string, wipe bool) {
	root := machineSkillRoot(agentID)
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no skills pushed — nothing to stage
	}
	target := filepath.Join(workdir, subdir)
	if wipe {
		_ = os.RemoveAll(target)
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		src := filepath.Join(root, ent.Name())
		dst := filepath.Join(target, ent.Name())
		_ = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil // never follow symlinks
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			out := filepath.Join(dst, rel)
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return nil
			}
			_ = os.WriteFile(out, b, 0o644)
			return nil
		})
	}
}

// machineProcWorkdir is where a processor run works on the machine.
func machineProcWorkdir(runID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "runs", "proc", runID)
}

// scratchWorkdir derives the local workdir for a scratch-domain run
// (mirrors the daemon's layout: runs/scratch/<domain>/goals/<goalID>,
// sub-goals under sg/<subGoalID>).
func scratchWorkdir(p link.RunDispatchParams) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, ".agentwork", "runs", "scratch", sanitizeName(p.DomainName), "goals", p.GoalID)
	if p.SubGoalID != "" {
		base = filepath.Join(base, "sg", p.SubGoalID)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	return base, nil
}

// buildEnv assembles the spawn environment: base + runtime/agent env +
// the run context (CLI 回调 + token) + a PATH entry pointing at an
// `agentwork` shim of this binary (the agent's terminal must find the CLI).
func (e *executor) buildEnv(p link.RunDispatchParams, workdir string) []string {
	env := os.Environ()
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	shimDir, err := ensureAgentworkShim()
	if err == nil && shimDir != "" {
		env = append(env, "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	env = append(env,
		"AGENTWORK_SERVER_URL="+e.serverURL,
		"AGENTWORK_GOAL_ID="+p.GoalID,
		"AGENTWORK_RUN_ID="+p.RunID,
		"AGENTWORK_AGENT_ID="+p.AgentID,
		"AGENTWORK_TOKEN="+p.Token,
	)
	return env
}

// ensureAgentworkShim symlinks this binary as `agentwork` under
// ~/.agentwork/bin so the agent's terminal has the command (the fixed
// block says "run `agentwork help`"). Returns the shim directory.
func ensureAgentworkShim() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".agentwork", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	shim := filepath.Join(dir, "agentwork")
	if _, err := os.Lstat(shim); err == nil {
		return dir, nil // exists (symlink or binary) — reuse
	}
	_ = os.Remove(shim)
	if err := os.Symlink(exe, shim); err != nil {
		// Fallback: copy the binary (some filesystems lack symlinks).
		if data, rerr := os.ReadFile(exe); rerr == nil {
			if werr := os.WriteFile(shim, data, 0o755); werr != nil {
				return "", werr
			}
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// callRPC wraps peer.Call with a bounded ctx (one-shot acks must not hang
// the executor forever).
func callRPC(peer *link.Peer, method string, params, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return peer.Call(ctx, method, params, out)
}

// sanitizeName strips path-hostile characters from a domain name for the
// local directory layout (mirrors the daemon's scratch sanitizer).
func sanitizeName(s string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}
	out := strings.Map(repl, s)
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		out = "domain"
	}
	return out
}
