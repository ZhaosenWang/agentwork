package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/service"
)

// machineDispatch is the daemon's half of the machine link (CLI 分支
// Phase 2): a registry of live /connect peers keyed by machine id, the
// run dispatch send, the ingestion of claimed/event-batch/finished
// uploads, and the watchdog for dispatched runs.

// RegisterMachinePeer binds a machine's live /connect peer (the server
// calls this after a successful machine.register).
func (d *Daemon) RegisterMachinePeer(machineID string, p *link.Peer) {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	if d.machinePeers == nil {
		d.machinePeers = map[string]*link.Peer{}
	}
	d.machinePeers[machineID] = p
}

// UnregisterMachinePeer drops a machine's peer (its link died).
func (d *Daemon) UnregisterMachinePeer(machineID string) {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	delete(d.machinePeers, machineID)
}

// MachinePeer returns the machine's live link peer (nil = offline).
func (d *Daemon) MachinePeer(machineID string) *link.Peer {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	return d.machinePeers[machineID]
}

// dispatchToMachine sends an assembled run over the machine's link and
// starts its watchdog. On missing peer or rejection the run fails with a
// clear reason (the claim gate keeps offline machines from claiming in the
// first place — this is the narrow race where the machine dropped between
// claim and dispatch).
func (d *Daemon) dispatchToMachine(ctx context.Context, q *service.ClaimedRow, p link.RunDispatchParams, machineID string) {
	peer := d.MachinePeer(machineID)
	if peer == nil {
		d.failRun(ctx, q, fmt.Sprintf("machine offline — the machine must run `agentwork connect` (machine %s)", machineID))
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var res link.RunDispatchResult
	if err := peer.Call(callCtx, link.MethodRunDispatch, p, &res); err != nil {
		d.failRun(ctx, q, fmt.Sprintf("dispatch: %v", err))
		return
	}
	if !res.Accepted {
		d.failRun(ctx, q, "dispatch rejected: "+res.Reason)
		return
	}
	d.machineLastEventMu.Lock()
	d.machineLastEvent[q.RunID] = time.Now()
	d.machineLastEventMu.Unlock()
	go d.runMachineWatchdog(q.RunID, machineID)
	logging.Infof("dispatch: run %s → machine %s (role=%s, attempt=%d)", q.RunID, machineID, p.Role, q.Attempt)
}

// IngestRunClaimed — the machine's run.claimed ack: publish the bus event
// (the frontend's queued→running flip and the review window's "审查中"
// state hang on it).
func (d *Daemon) IngestRunClaimed(ctx context.Context, p link.RunClaimedParams) *link.RPCError {
	var goalID, agentID string
	var attempt int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT goal_id, agent_id, attempt FROM run WHERE id=?`, p.RunID).Scan(&goalID, &agentID, &attempt); err != nil {
		return &link.RPCError{Code: link.CodeInternal, Message: "unknown run"}
	}
	d.bus.Publish(ctx, events.Event{Topic: "run:claimed", Payload: map[string]any{
		"run_id": p.RunID, "goal_id": goalID, "agent_id": agentID,
		"attempt": attempt, "started_at": time.Now().UTC().Format(time.RFC3339Nano),
	}})
	logging.Infof("machine: run %s claimed (remote execution started)", p.RunID)
	return nil
}

// IngestRunEvents persists a batch of stream events — the machine's
// per-run seq, contiguous within the batch — exactly like the local
// executor's persistEvent, refreshes the watchdog's activity stamp, and
// broadcasts run:event for the live panels.
func (d *Daemon) IngestRunEvents(ctx context.Context, p link.RunEventBatchParams) *link.RPCError {
	if p.RunID == "" || len(p.Events) == 0 {
		return nil
	}
	// Gap detection: the daemon expects contiguous seq per run. A gap means
	// frames were lost (link degradation) — logged; the transcript refill
	// (loadSession) lands in a later phase.
	d.machineLastEventMu.Lock()
	last, seen := d.machineLastSeq[p.RunID]
	if !seen {
		// First batch: the machine's seq starts at 1 — no gap to judge.
	} else if p.SeqStart != last+1 {
		logging.Infof("machine: run %s event seq gap (have %d, got %d) — %d event(s) lost on the wire", p.RunID, last, p.SeqStart, p.SeqStart-last-1)
	}
	d.machineLastSeq[p.RunID] = p.SeqStart + int64(len(p.Events)) - 1
	d.machineLastEvent[p.RunID] = time.Now()
	d.machineLastEventMu.Unlock()

	for _, ev := range p.Events {
		pev := proto.Event{Type: proto.EventType(ev.Kind), Text: ev.Text, Tool: ev.Tool, Input: ev.Input, Output: ev.Output}
		d.persistEvent(ctx, p.RunID, pev)
		d.bus.Publish(ctx, events.Event{Topic: "run:event", Payload: map[string]any{
			"run_id": p.RunID, "event": pev,
		}})
	}
	return nil
}

// IngestRunFinished — the machine's terminal report. The platform is the
// only authority over run status: the report flows through the normal
// Finish (conditional stamp + reconcile). Before that, a COMPLETED repo
// run gets its gate evaluation (the daemon computes, the goal layer
// judges — the same division as the local path): the machine's transferred
// branch is adopted and diffed, and the fired gates land on the run row.
func (d *Daemon) IngestRunFinished(ctx context.Context, p link.RunFinishedParams) *link.RPCError {
	if p.RunID == "" {
		return &link.RPCError{Code: link.CodeInvalidParams, Message: "run_id is required"}
	}
	if p.Status == "completed" {
		// The platform verifies (invariant 9 — the worker never verifies
		// its own work): setup+verify+guards run on the adopted branch,
		// and a red result flips the report to failed (the local path's
		// semantics: verification failure = run failure + retry chain).
		if failReport := d.processMachineRunCompletion(ctx, p.RunID, p.Summary); failReport != "" {
			p.Status = "failed"
			p.Summary = failReport
		}
	}
	d.flushRunMessages(ctx, p.RunID)
	d.machineLastEventMu.Lock()
	delete(d.machineLastSeq, p.RunID)
	delete(d.machineLastEvent, p.RunID)
	d.machineLastEventMu.Unlock()
	if err := d.runSvc.Finish(ctx, p.RunID, p.Status, p.Summary); err != nil && !errors.Is(err, service.ErrRunAlreadyTerminal) {
		logging.Infof("machine: finish run %s: %v", p.RunID, err)
		return &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
	}
	logging.Infof("machine: run %s finished (%s)", p.RunID, p.Status)
	return nil
}

// runMachineWatchdog supervises a dispatched run: no events for idleWindow
// or a total duration beyond maxRunDuration cancels it via the machine
// link (the local executor's watchdog semantics, re-applied across the
// wire).
func (d *Daemon) runMachineWatchdog(runID, machineID string) {
	tick := time.NewTicker(idleWindow / 2)
	defer tick.Stop()
	start := time.Now()
	for {
		<-tick.C
		// Terminal? The run's state is the truth — the watchdog dies when
		// its run is no longer running.
		var status string
		if err := d.st.DB().QueryRowContext(context.Background(),
			`SELECT status FROM run WHERE id=?`, runID).Scan(&status); err != nil || status != "running" {
			return
		}
		d.machineLastEventMu.Lock()
		last, ok := d.machineLastEvent[runID]
		d.machineLastEventMu.Unlock()
		fired := false
		reason := ""
		if ok && time.Since(last) > idleWindow {
			fired, reason = true, "idle_watchdog"
		} else if time.Since(start) > 2*time.Hour {
			fired, reason = true, "max_run_duration"
		}
		if !fired {
			continue
		}
		logging.Infof("machine: watchdog firing for run %s (%s)", runID, reason)
		if peer := d.MachinePeer(machineID); peer != nil {
			_ = peer.Notify(context.Background(), link.MethodRunCancel, link.RunCancelParams{RunID: runID, Reason: reason})
		}
		// The machine reports the terminal state via run.finished; if the
		// link is dead, stamp locally so the run cannot hang forever.
		if d.MachinePeer(machineID) == nil {
			_ = d.runSvc.Finish(context.Background(), runID, "cancelled", "machine offline — watchdog cancelled")
		}
		return
	}
}

// processMachineRunCompletion is the daemon-side completion processing for
// a machine-executed run: adopt the transferred branch (retrying for the
// git host's ref-visibility delay), run setup+verify+guards on an ephemeral
// worktree, evaluate the gates (goal-level), and stamp refs/evidence/
// gates_hit. Returns a failure report when verification or guards fail —
// the caller flips the run to failed. The daemon computes, the goal layer
// judges; the worker never verifies its own work (invariant 9).
func (d *Daemon) processMachineRunCompletion(ctx context.Context, runID, agentSummary string) string {
	var goalID, role, subGoalID, domainID, gitURL, defaultBranch, gitCredentials, domainType string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.goal_id, r.role, r.sub_goal_id, d.id, d.git_url, d.default_branch, d.git_credentials, COALESCE(d.type,'')
		 FROM run r JOIN goal g ON g.id = r.goal_id JOIN domain d ON d.id = g.domain_id WHERE r.id=?`,
		runID).Scan(&goalID, &role, &subGoalID, &domainID, &gitURL, &defaultBranch, &gitCredentials, &domainType); err != nil {
		return "" // unknown run / no domain — the goal layer's judgment stands
	}
	if domainType == "scratch" || role == "consult" || role == "review" {
		return "" // scratch: no diff; read-only roles: nothing to verify or gate
	}
	checks, timeout, baseline, frozen := d.loadDomainChecks(ctx, domainID)
	if !frozen {
		// Unfrozen policy: no verification against an unconfirmed definition;
		// the goal layer forces the human checkpoint.
		return ""
	}
	subGoalRun := role == "subgoal"
	verifyRun := role == "verify"
	if verifyRun {
		return "" // the verifier's own run has no policy verification
	}

	unlock := d.lockDomain(domainID)
	defer unlock()
	repo := domainRepoPath(domainID)
	if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
		logging.Infof("machine: completion %s: prepare repo: %v", runID, err)
		return ""
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	branchName := goalBranchName(goalID)
	if subGoalRun {
		branchName = subGoalBranchName(goalID, subGoalID)
	}

	// Adopt the transferred branch (retries cover the git host's ref
	// visibility delay — live: a fetch at finished+0ms missed a push that
	// was already on the remote). A local ref already present wins.
	adopted := false
	if _, err := gitRun(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branchName); err == nil {
		adopted = true
	}
	for attempt := 0; !adopted && attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(2 * time.Second):
			}
		}
		if _, err := gitRun(ctx, repo, "fetch", "origin"); err != nil {
			continue
		}
		if _, err := gitRun(ctx, repo, "branch", branchName, "origin/agentwork/"+branchName); err == nil {
			adopted = true
		}
	}
	// Verification target: the adopted branch, or origin/<default> when no
	// transfer exists (a zero-change run — the state equals the base; the
	// local path runs verification on zero-change runs too).
	target := branchName
	if !adopted {
		target = "origin/" + defaultBranch
	}
	wt := filepath.Join(runsRoot(), "gates", runID)
	if _, err := gitRun(ctx, repo, "worktree", "add", "--force", "--detach", wt, target); err != nil {
		logging.Infof("machine: completion %s: worktree: %v", runID, err)
		return ""
	}
	defer func() {
		_, _ = gitRun(context.Background(), repo, "worktree", "remove", "--force", wt)
	}()

	// The diff base: sub-goal runs measure against the goal branch's tip
	// (their integration base); goal-level runs against origin/<default>.
	base := strings.TrimSpace(mustGit(ctx, repo, "rev-parse", "origin/"+defaultBranch))
	if subGoalRun {
		if mb, err := gitRun(ctx, wt, "merge-base", "origin/"+defaultBranch, "HEAD"); err == nil && strings.TrimSpace(mb) != "" {
			base = strings.TrimSpace(mb)
		}
	}

	// Verification + guards — the local path's semantics: red = run failed.
	var verifyReport, guardReport string
	verifyReport, ok, policyIssue := runVerification(ctx, wt, checks, timeout)
	if !ok {
		if policyIssue {
			d.annotatePolicyIssue(ctx, goalID)
		}
		return "verification failed:\n" + verifyReport
	}
	// Guards measure the run's own diff (base..HEAD) — repo-only, and
	// scratch returned early, so they always run here.
	guardReport, ok = checkGuards(ctx, wt, base, checks, baseline)
	if !ok {
		return "guards failed:\n" + guardReport
	}

	// Gates (goal-level only, mirroring the local path) + refs + evidence.
	gatesHit := []string{}
	if !subGoalRun && !verifyRun {
		gatesHit = evalGates(ctx, wt, base, checks)
	}
	head := strings.TrimSpace(mustGit(ctx, wt, "rev-parse", "HEAD"))
	ev := buildEvidence(ctx, wt, base, agentSummary, verifyReport, guardReport)
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET gates_hit=?, base_ref=?, head_ref=?, evidence=? WHERE id=?`,
		mustJSON(gatesHit), base, head, ev, runID); err != nil {
		logging.Infof("machine: completion %s: stamp: %v", runID, err)
		return ""
	}
	logging.Infof("machine: completion %s: verified ok, %d gate(s) fired", runID, len(gatesHit))
	return ""
}

// mustGit runs git in dir and returns the output, trimmed ('' on error) —
// judgment paths must not crash on a missing ref.
func mustGit(ctx context.Context, dir string, args ...string) string {
	out, err := gitRun(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ── config push (CLI 分支 Phase 4) ──

// PushMachineSkills full-syncs every agent whose runtime belongs to the
// machine (called on register — edits made while the machine was offline
// land here; the overwrite is idempotent).
func (d *Daemon) PushMachineSkills(ctx context.Context, machineID string) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT a.id FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE r.machine_id=?`, machineID)
	if err != nil {
		logging.Infof("machine: skills sync %s: %v", machineID, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		d.PushAgentSkills(ctx, id)
	}
}

// PushAgentSkills sends the agent's selected skill packages to its machine
// via config.push. No-op for local/legacy runtimes and offline machines
// (the register-time full sync covers those).
func (d *Daemon) PushAgentSkills(ctx context.Context, agentID string) {
	var skillsJSON, runtimeID, machineID, systemPrompt string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT a.skills, a.runtime_id, r.machine_id, a.system_prompt FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`,
		agentID).Scan(&skillsJSON, &runtimeID, &machineID, &systemPrompt); err != nil || machineID == "" {
		return // local/legacy runtime — nothing to push
	}
	peer := d.MachinePeer(machineID)
	if peer == nil {
		return // offline — the register-time sync replays it
	}
	var skillIDs []string
	_ = json.Unmarshal([]byte(skillsJSON), &skillIDs)
	skillSvc := service.NewSkillService(d.st)
	var pushes []link.SkillPush
	for _, sid := range skillIDs {
		files, err := skillSvc.Files(ctx, sid)
		if err != nil {
			logging.Infof("machine: skills push %s: load %s: %v", agentID, sid, err)
			continue
		}
		var name string
		_ = d.st.DB().QueryRowContext(ctx, `SELECT name FROM skill WHERE id=?`, sid).Scan(&name)
		sp := link.SkillPush{Name: name}
		for _, path := range service.SortedFilePaths(files) {
			sp.Files = append(sp.Files, link.SkillFile{Path: path, Content: files[path]})
		}
		pushes = append(pushes, sp)
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var res link.ConfigPushResult
	if err := peer.Call(callCtx, link.MethodConfigPush, link.ConfigPushParams{
		AgentID:      agentID,
		SystemPrompt: systemPrompt,
		SkillsDir:    d.machineSkillsDir(ctx, machineID, runtimeID),
		Skills:       pushes,
	}, &res); err != nil {
		logging.Infof("machine: skills push %s: %v", agentID, err)
		return
	}
	if len(res.Errors) > 0 {
		logging.Infof("machine: skills push %s: %d error(s): %v", agentID, len(res.Errors), res.Errors)
		return
	}
	logging.Infof("machine: skills push %s: %d skill(s) installed", agentID, len(res.Written))
}

// machineSkillsDir resolves the CLI's skills directory from the machine's
// probe report (the probe stored it at register time); '' = the CLI's
// default fallback (~/.claude/skills).
func (d *Daemon) machineSkillsDir(ctx context.Context, machineID, runtimeID string) string {
	var probedJSON string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT probed_clis FROM machine WHERE id=?`, machineID).Scan(&probedJSON); err != nil {
		return ""
	}
	var clis []link.ProbeCLI
	if err := json.Unmarshal([]byte(probedJSON), &clis); err != nil {
		return ""
	}
	var spawnJSON string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT args FROM runtime WHERE id=?`, runtimeID).Scan(&spawnJSON); err != nil {
		return ""
	}
	var spawn []string
	_ = json.Unmarshal([]byte(spawnJSON), &spawn)
	cliName := ""
	if len(spawn) > 0 {
		cliName = spawn[0]
	}
	for _, c := range clis {
		if c.Name == cliName {
			return c.SkillsDir
		}
	}
	return ""
}
