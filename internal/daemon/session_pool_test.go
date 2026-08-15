package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ── Session worktree state machine (决策 6-21) ──
//
// ONE worktree per (agent, goal) session, re-pointed per wake: owner holds
// the goal branch, subgoal the sub-goal branch, guests detached snapshots.
// Same target refreshes, a different target switches the checkout.

// seedSessionRepo builds a source repo with one commit on main.
func seedSessionRepo(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	mustRunGit(t, "", "init", "-b", "main", src)
	mustRunGit(t, src, "config", "user.email", "t@t")
	mustRunGit(t, src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, src, "add", ".")
	mustRunGit(t, src, "commit", "-m", "init")
	return src
}

func newTestLiveSession(goalID, agentID, domainID, src string) *liveSession {
	return &liveSession{
		key: sessionKey(agentID, goalID), goalID: goalID, agentID: agentID,
		domainID: domainID, gitURL: "file://" + src, defaultBranch: "main",
	}
}

func branchOf(t *testing.T, wt string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v %s", err, out)
	}
	return string(out)
}

// TestSessionWorktreeSwitchesPerWake: owner → subgoal → owner — the ONE
// worktree path follows the wake's target; the process never moves.
func TestSessionWorktreeSwitchesPerWake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	src := seedSessionRepo(t)
	d := &Daemon{}
	ls := newTestLiveSession("goal-1", "agent-1", "dom-1", src)

	// Owner wake: the goal branch is born at origin/main.
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("owner", "")); err != nil {
		t.Fatalf("owner wake: %v", err)
	}
	if b := branchOf(t, ls.workdir); b != goalBranchName("goal-1")+"\n" {
		t.Fatalf("owner checkout must hold the goal branch, got %q", b)
	}

	// Sub-goal wake: the checkout switches to the sub-goal branch (born at
	// the goal branch).
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("subgoal", "sg-1")); err != nil {
		t.Fatalf("subgoal wake: %v", err)
	}
	if b := branchOf(t, ls.workdir); b != subGoalBranchName("goal-1", "sg-1")+"\n" {
		t.Fatalf("subgoal checkout must hold the sub-goal branch, got %q", b)
	}

	// Back to the owner: the checkout returns to the goal branch (plain add
	// — the branch exists).
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("owner", "")); err != nil {
		t.Fatalf("owner wake #2: %v", err)
	}
	if b := branchOf(t, ls.workdir); b != goalBranchName("goal-1")+"\n" {
		t.Fatalf("the owner checkout must return, got %q", b)
	}
}

// TestSessionWorktreeSameTargetRefreshes: a consecutive same-target wake
// (the sub-goal RETRY) does NOT switch — the checkout (and its dirt) stays.
func TestSessionWorktreeSameTargetRefreshes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	src := seedSessionRepo(t)
	d := &Daemon{}
	ls := newTestLiveSession("goal-1", "agent-1", "dom-1", src)

	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("subgoal", "sg-1")); err != nil {
		t.Fatalf("subgoal wake: %v", err)
	}
	wt := ls.workdir
	// The retry's uncommitted dirt must survive a same-target wake (the
	// session owns it).
	if err := os.WriteFile(filepath.Join(wt, "WIP.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("subgoal", "sg-1")); err != nil {
		t.Fatalf("retry wake: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "WIP.txt")); err != nil {
		t.Fatalf("the retry's dirt must survive a same-target wake: %v", err)
	}
	if b := branchOf(t, wt); b != subGoalBranchName("goal-1", "sg-1")+"\n" {
		t.Fatalf("the retry keeps the sub-goal checkout, got %q", b)
	}
}

// TestSessionWorktreeRecoversFromLeak: a half-prepared dir (a failed -b
// add's debris — no .git file) and a leaked registered checkout both
// recover on the next acquire (live: "branch exists" + "path exists"
// stacked on every retry).
func TestSessionWorktreeRecoversFromLeak(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	src := seedSessionRepo(t)
	d := &Daemon{}
	ls := newTestLiveSession("goal-1", "agent-1", "dom-1", src)

	// First creation succeeds — the branch is born with the checkout.
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("owner", "")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// SIMULATE the leak: the worktree stays registered + the dir stays.
	wt := sessionWorktreePath("goal-1", "agent-1")
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("the worktree must exist after create: %v", err)
	}
	// A same-target wake on a leaked checkout must just refresh (the
	// checkout IS there).
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("owner", "")); err != nil {
		t.Fatalf("same-target wake after leak: %v", err)
	}

	// The half-prepared shape: delete .git (the failed-add debris) — the
	// same-target wake must now fall through to a clear + re-add.
	if err := os.RemoveAll(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureSessionWorktree(ctx, ls, wakeTargetFor("owner", "")); err != nil {
		t.Fatalf("recovery from a half-prepared dir: %v", err)
	}
	if b := branchOf(t, wt); b != goalBranchName("goal-1")+"\n" {
		t.Fatalf("the recovered worktree must hold the goal branch, got %q", b)
	}
}

// TestSessionCapable: every worker role of an ACP runtime rides the
// session; non-ACP runtimes keep the per-run path.
func TestSessionCapable(t *testing.T) {
	if !sessionCapable("acp") {
		t.Fatal("acp runtimes always ride sessions (决策 6-21)")
	}
	if sessionCapable("jsonl") || sessionCapable("jsonrpc") || sessionCapable("") {
		t.Fatal("non-acp runtimes keep the per-run path")
	}
}
