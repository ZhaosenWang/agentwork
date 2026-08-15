package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/mcp"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/runtime"
	"github.com/google/uuid"
)

// ── Persistent sessions (决策 6-21) ──
//
// One (agent, goal) pair keeps ONE process + ONE ACP session alive across
// its turns. The session owns a session-scoped worktree: the owner session
// holds the goal branch's checkout across runs (its dirt belongs to the
// session — the same agent), guest sessions (review/consult) hold a detached
// snapshot refreshed at every wake. The workspace MCP server and the client
// RPC handler are bound ONCE per session (registered under the session key);
// each wake only re-points them at the current run/worktree.

// liveSession is one pooled (agent, goal) session.
type liveSession struct {
	key             string // agentID/goalID — also the /mcp/{key} registration id
	goalID, agentID string
	role            string // the session's run role (owner|review|consult)

	sess proto.Session
	conn proto.Conn

	scratch        bool
	domainID       string
	domainName     string
	gitURL         string
	gitCredentials string
	defaultBranch  string
	// workdir is the session's ONE worktree path — re-pointed per wake to
	// the wake's target (决策 6-21: all worker roles ride the session; the
	// checkout switches, the process and MCP server stay).
	workdir string
	// heldKind/heldSubGoalID record what the checkout currently holds
	// (owner|subgoal|guest) so the next wake knows whether to refresh or
	// switch. scratchCopy marks a guest snapshot dir we may remove on evict
	// (goal/sg dirs are the DELIVERABLES — never removed).
	heldKind      string
	heldSubGoalID string
	scratchCopy   bool

	env     *runEnvironment
	mcpExec *mcp.Executor
	// mcpID is the route-safe id the workspace MCP server registers under
	// (/mcp/{mcpID} — ONE path segment; the pool key contains a slash and
	// would break the route). Generated per session, advertised at
	// session/new.
	mcpID string

	lastRunEndedAt string // the feed delta's cutoff (决策 6-21)
}

func sessionKey(agentID, goalID string) string { return agentID + "/" + goalID }

// sessionWorktreePath is the persistent checkout a session works in.
func sessionWorktreePath(goalID, agentID string) string {
	return filepath.Join(runsRoot(), "sessions", goalID, agentID)
}

// sessionMu guards the pool map (a dedicated lock — d.mu already gates many
// hot paths; the pool's acquire/evict must not contend with them).
var sessionMu sync.Mutex

// sessions returns the daemon's pool (created lazily — test constructions
// build Daemon literals without New).
func (d *Daemon) sessions() map[string]*liveSession {
	d.mu.Lock()
	if d.sessionPool == nil {
		d.sessionPool = make(map[string]*liveSession)
	}
	m := d.sessionPool
	d.mu.Unlock()
	return m
}

// sessionCapable reports whether this run rides a persistent session
// (决策 6-21): ACP provider (mandatory — no capability detection; a runtime
// without ACP is out of scope). EVERY worker role rides the session — the
// checkout switches per wake, the process and MCP server do not (the
// per-run path survives only for non-ACP runtimes).
func sessionCapable(provider string) bool {
	return provider == "acp"
}

// wakeTarget describes what the session worktree must hold for this wake.
type wakeTarget struct {
	kind      string // owner | subgoal | guest
	subGoalID string // '' for goal-level wakes (guest verify carries its sub-goal)
}

func wakeTargetFor(role, subGoalID string) wakeTarget {
	switch role {
	case "owner":
		return wakeTarget{kind: "owner"}
	case "subgoal":
		return wakeTarget{kind: "subgoal", subGoalID: subGoalID}
	default: // review | consult | verify — read-only snapshots
		return wakeTarget{kind: "guest", subGoalID: subGoalID}
	}
}

// acquireSession returns the (agent, goal) session, creating it on first
// use. isNew tells the prompt builder whether the session has no memory
// (full contract + full feed) or continues one (delta). The worktree is
// (re-)pointed at the wake's target — same target refreshes, a different
// target switches the checkout (the process and MCP server stay).
func (d *Daemon) acquireSession(ctx context.Context, runID, goalID, agentID, provider, role, subGoalID string,
	scratch bool, domainName, domainID, gitURL, gitCredentials, defaultBranch, serverURL string,
	transport, execPath, endpoint string, args []string, rtEnv map[string]string, taskEnv []string) (*liveSession, bool, error) {

	key := sessionKey(agentID, goalID)
	target := wakeTargetFor(role, subGoalID)
	sessionMu.Lock()
	defer sessionMu.Unlock()
	pool := d.sessions()
	if ls, ok := pool[key]; ok {
		// Wake #2+: (re-)point the checkout at the wake's target.
		if err := d.ensureSessionWorktree(ctx, ls, target); err != nil {
			// The worktree is unrecoverable (branch deleted mid-flight?) —
			// evict and fall through to a fresh session.
			logging.Infof("daemon: session %s worktree refresh failed: %v — replacing", key, err)
			d.evictSessionLocked(key)
		} else {
			return ls, false, nil
		}
	}

	// Fresh session: point the worktree at the wake's target first (the
	// Open's Cwd and the executor both anchor on it), then the transport +
	// protocol session.
	ls := &liveSession{
		key: key, goalID: goalID, agentID: agentID, role: role,
		scratch: scratch, domainID: domainID, domainName: domainName,
		gitURL: gitURL, gitCredentials: gitCredentials, defaultBranch: defaultBranch,
	}
	if err := d.ensureSessionWorktree(ctx, ls, target); err != nil {
		return nil, false, err
	}
	workdir := ls.workdir

	// From here on, any failure must release the worktree we just created —
	// a leaked checkout blocks the next acquire (the live failure: branch
	// exists + path exists on the retry).
	partial := &liveSession{scratch: scratch, role: role, domainID: domainID, workdir: workdir}
	releaseOnErr := func() {
		d.releaseSessionWorktree(partial)
	}

	// The session-scoped execution environment: ONE client RPC handler + ONE
	// workspace MCP server for the session's whole life.
	env := newRunEnvironment(runID, goalID, agentID, workdir, serverURL)
	mcpExec := mcp.NewExecutor(workdir, env.runEnv(nil), env.tm)
	mcpExec.SetCollaboration(goalID, agentID, runID, d.commentSvc, d.goalSvc, d.runSvc, d.agentSvc, d.squadSvc)
	mcpID := uuid.NewString()
	d.mu.Lock()
	d.mcpExecs[mcpID] = mcpExec
	d.mu.Unlock()

	spec := runtime.Spec{
		Transport:  transport,
		Executable: execPath,
		Args:       args,
		Endpoint:   endpoint,
		Env:        rtEnv,
		Cwd:        workdir,
	}
	conn, err := runtime.Open(ctx, spec, taskEnv)
	if err != nil {
		d.mu.Lock()
		delete(d.mcpExecs, mcpID)
		d.mu.Unlock()
		releaseOnErr()
		return nil, false, fmt.Errorf("open transport: %w", err)
	}

	be, err := d.protoReg.Get(provider)
	if err != nil {
		conn.Close()
		d.mu.Lock()
		delete(d.mcpExecs, mcpID)
		d.mu.Unlock()
		releaseOnErr()
		return nil, false, fmt.Errorf("provider %q: %w", provider, err)
	}
	sb, ok := be.(proto.SessionBackend)
	if !ok {
		conn.Close()
		d.mu.Lock()
		delete(d.mcpExecs, mcpID)
		d.mu.Unlock()
		releaseOnErr()
		return nil, false, fmt.Errorf("provider %q has no session backend (决策 6-21: ACP is mandatory)", provider)
	}
	sess, err := sb.OpenSession(ctx, proto.SessionSpec{
		Conn:          conn,
		Cwd:           workdir,
		ClientHandler: env,
		McpServers:    append([]acp.McpServer{{Type: "http", Name: "agentwork", URL: serverURL + "/mcp/" + mcpID}}, d.extraMcpServers(ctx, agentID)...),
	})
	if err != nil {
		conn.Close()
		d.mu.Lock()
		delete(d.mcpExecs, mcpID)
		d.mu.Unlock()
		releaseOnErr()
		return nil, false, fmt.Errorf("open session: %w", err)
	}

	ls.sess, ls.conn, ls.env, ls.mcpExec, ls.mcpID = sess, conn, env, mcpExec, mcpID
	pool[key] = ls
	logging.Infof("daemon: session %s opened (role=%s, workdir=%s)", key, role, workdir)
	return ls, true, nil
}

// ensureSessionWorktree (re-)points the session's single worktree at the
// wake's target (决策 6-21): the checkout switches between the goal branch
// (owner), the sub-goal branch (subgoal) and detached snapshots (guests),
// while the process and the MCP server never move.
//
// Same target = refresh (owner/subgoal: the checkout tracks its branch and
// only fetch is needed — dirt belongs to the session; guests: reset the
// snapshot to the branch tip). Different target = remove + re-add.
// Leftovers from failed attempts (a half-prepared dir, a leaked registered
// checkout) are cleared before every add.
func (d *Daemon) ensureSessionWorktree(ctx context.Context, ls *liveSession, t wakeTarget) error {
	if ls.scratch {
		return d.ensureScratchWorktree(ctx, ls, t)
	}
	unlock := d.lockDomain(ls.domainID)
	defer unlock()
	if err := d.ensureSharedRepo(ctx, ls.domainID, ls.gitURL, ls.gitCredentials); err != nil {
		return err
	}
	repo := domainRepoPath(ls.domainID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	wt := sessionWorktreePath(ls.goalID, ls.agentID)
	_ = os.MkdirAll(filepath.Dir(wt), 0o755)
	ls.workdir = wt

	// Same target AND a live checkout → refresh only.
	if ls.heldKind == t.kind && ls.heldSubGoalID == t.subGoalID {
		if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
			if t.kind == "guest" {
				ref, err := d.targetRef(ctx, repo, ls.goalID, t.subGoalID, ls.defaultBranch)
				if err != nil {
					return err
				}
				out, err := exec.CommandContext(ctx, "git", "-C", wt, "reset", "--hard", ref).CombinedOutput()
				if err != nil {
					return fmt.Errorf("reset guest snapshot: %w: %s", err, strings.TrimSpace(string(out)))
				}
			}
			return nil
		}
		// A half-prepared dir from a failed add — fall through to a switch.
	}

	// Switch (or recover): clear whatever the path holds, then add the target.
	if err := d.clearSessionCheckout(ctx, repo, wt); err != nil {
		return err
	}
	branch := ls.targetBranch(t)
	if t.kind == "guest" {
		ref, err := d.targetRef(ctx, repo, ls.goalID, t.subGoalID, ls.defaultBranch)
		if err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "--detach", wt, ref).CombinedOutput()
		if err != nil {
			_ = d.clearSessionCheckout(ctx, repo, wt)
			return fmt.Errorf("git worktree add (guest session): %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		var out []byte
		var err error
		if _, verr := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).CombinedOutput(); verr == nil {
			// The branch exists (a prior run/crash created it) — plain checkout.
			out, err = exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", wt, branch).CombinedOutput()
		} else {
			// The branch is born at the wake's natural base.
			base, berr := d.targetBase(ctx, repo, ls.goalID, t, ls.defaultBranch)
			if berr != nil {
				_ = d.clearSessionCheckout(ctx, repo, wt)
				return berr
			}
			out, err = exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "-b", branch, wt, base).CombinedOutput()
		}
		if err != nil {
			_ = d.clearSessionCheckout(ctx, repo, wt)
			return fmt.Errorf("git worktree add (session %s): %w: %s", t.kind, err, strings.TrimSpace(string(out)))
		}
	}
	ls.heldKind, ls.heldSubGoalID = t.kind, t.subGoalID
	return nil
}

// targetBranch names the branch a non-guest target checks out.
func (ls *liveSession) targetBranch(t wakeTarget) string {
	if t.kind == "subgoal" {
		return subGoalBranchName(ls.goalID, t.subGoalID)
	}
	return goalBranchName(ls.goalID)
}

// targetRef resolves the ref a guest snapshot detaches at (sub-goal branch
// for verify, goal branch for review/consult), with the origin/<default>
// fallback when the branch does not exist yet.
func (d *Daemon) targetRef(ctx context.Context, repo, goalID, subGoalID, defaultBranch string) (string, error) {
	branch := goalBranchName(goalID)
	if subGoalID != "" {
		branch = subGoalBranchName(goalID, subGoalID)
	}
	if _, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).CombinedOutput(); err == nil {
		return "refs/heads/" + branch, nil
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return "origin/" + defaultBranch, nil
}

// targetBase resolves where a NEW branch is born: the sub-goal branch from
// the goal branch (origin/<default> fallback), the goal branch from
// origin/<default>.
func (d *Daemon) targetBase(ctx context.Context, repo, goalID string, t wakeTarget, defaultBranch string) (string, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if t.kind == "subgoal" {
		if _, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+goalBranchName(goalID)).CombinedOutput(); err == nil {
			return "refs/heads/" + goalBranchName(goalID), nil
		}
	}
	return "origin/" + defaultBranch, nil
}

// clearSessionCheckout removes whatever the session path holds: the
// registration (a leaked checkout), the dir (a half-prepared add), and the
// prunable metadata. Idempotent — the next add runs on a clean path.
func (d *Daemon) clearSessionCheckout(ctx context.Context, repo, wt string) error {
	if _, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "remove", "--force", wt).CombinedOutput(); err == nil {
		logging.Infof("daemon: cleared leaked session worktree %s", wt)
	}
	_ = os.RemoveAll(wt)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune").CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree prune: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureScratchWorktree maps the wake target onto the scratch domain's dirs:
// owner → the goal dir (the deliverable), subgoal → its sg/ subdir (the
// deliverable), guests → a session-scoped COPY refreshed per wake. Goal and
// sg dirs are the deliverable — never removed, never re-copied.
func (d *Daemon) ensureScratchWorktree(ctx context.Context, ls *liveSession, t wakeTarget) error {
	switch t.kind {
	case "owner":
		ls.workdir = scratchGoalDir(ls.domainName, ls.goalID)
		return os.MkdirAll(ls.workdir, 0o755)
	case "subgoal":
		ls.workdir = filepath.Join(scratchGoalDir(ls.domainName, ls.goalID), "sg", t.subGoalID)
		return os.MkdirAll(ls.workdir, 0o755)
	default: // guest — a fresh copy per wake
		src := scratchGoalDir(ls.domainName, ls.goalID)
		if t.subGoalID != "" {
			src = filepath.Join(src, "sg", t.subGoalID)
		}
		wt := sessionWorktreePath(ls.goalID, ls.agentID)
		_ = os.RemoveAll(wt)
		ls.workdir = wt
		ls.scratchCopy = true
		if err := copyDir(src, wt); err != nil {
			logging.Infof("daemon: session scratch snapshot %s: %v", ls.key, err)
			return os.MkdirAll(wt, 0o755)
		}
		return nil
	}
}

// evictSessionLocked closes one session and forgets it (caller holds
// sessionMu). The worktree is released (git) / removed (scratch copy).
func (d *Daemon) evictSessionLocked(key string) {
	pool := d.sessions()
	ls, ok := pool[key]
	if !ok {
		return
	}
	delete(pool, key)
	d.mu.Lock()
	delete(d.mcpExecs, ls.mcpID)
	d.mu.Unlock()
	_ = ls.sess.Close()
	d.releaseSessionWorktree(ls)
	logging.Infof("daemon: session %s closed", key)
}

// releaseSessionWorktree frees the session's checkout: the goal branch's
// single checkout must be released for the next owner (handoff/restart);
// scratch guest copies are just removed.
func (d *Daemon) releaseSessionWorktree(ls *liveSession) {
	if ls.workdir == "" {
		return
	}
	if ls.scratch {
		// Only the guest snapshot COPY is ours — the goal dir and sg/ subdirs
		// are the DELIVERABLES and survive the session.
		if ls.scratchCopy {
			_ = os.RemoveAll(ls.workdir)
		}
		return
	}
	unlock := d.lockDomain(ls.domainID)
	defer unlock()
	if out, err := exec.CommandContext(context.Background(), "git", "-C", domainRepoPath(ls.domainID), "worktree", "remove", "--force", ls.workdir).CombinedOutput(); err != nil {
		logging.Infof("daemon: release session worktree %s: %v %s", ls.key, err, strings.TrimSpace(string(out)))
	}
}

// evictSessionKey closes one session by key (public wrapper — the caller
// does not hold sessionMu).
func (d *Daemon) evictSessionKey(key string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	d.evictSessionLocked(key)
}

// closeGoalSessions closes every session of a goal (goal terminal/deleted).
func (d *Daemon) closeGoalSessions(goalID string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	pool := d.sessions()
	for key, ls := range pool {
		if ls.goalID == goalID {
			d.evictSessionLocked(key)
		}
	}
}

// closeOwnerSession closes the owner session for a goal (handoff — the new
// owner needs the goal branch's single checkout).
func (d *Daemon) closeOwnerSession(goalID string) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	pool := d.sessions()
	for key, ls := range pool {
		if ls.goalID == goalID && ls.role == "owner" {
			d.evictSessionLocked(key)
		}
	}
}

// closeAllSessions tears every pooled session down (daemon shutdown).
func (d *Daemon) closeAllSessions() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	pool := d.sessions()
	for key := range pool {
		d.evictSessionLocked(key)
	}
}
