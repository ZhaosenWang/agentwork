package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/link"
)

// seedRepo seeds a local bare repo with a commit on master and returns its
// file:// URL (the machine-side git flow runs against real git, no network).
func seedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	bare := filepath.Join(dir, "bare.git")
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
		}
	}
	run(dir, "init", "--bare", bare)
	run(dir, "init", work)
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "t")
	run(work, "symbolic-ref", "HEAD", "refs/heads/master")
	run(work, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "f.txt")
	run(work, "commit", "-m", "init")
	run(work, "push", "origin", "master")
	return "file://" + bare
}

// TestGitWorkdirAndTransferPush covers the machine-side git execution
// (Phase 3): the bare clone, the goal branch from origin/master, the
// worktree, the commit, and the agentwork/<branch> transfer push.
func TestGitWorkdirAndTransferPush(t *testing.T) {
	// The machine's layout lives under HOME — isolate it so parallel/test
	// reruns never see a leftover clone pointing at a deleted temp repo.
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	url := seedRepo(t)
	p := link.RunDispatchParams{
		RunID: "run-1", GoalID: "goal-1", AgentID: "a",
		DomainID: "dom-1", GitURL: url, DefaultBranch: "master",
		GitIdentity: "agentwork[bot] <bot@local>",
	}

	wt, repo, branch, cleanup, err := ensureGitWorkdir(ctx, p)
	if err != nil {
		t.Fatalf("workdir: %v", err)
	}
	defer cleanup()
	if branch != "feat-goal-1" {
		t.Fatalf("expected branch feat-goal-1, got %s", branch)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree must be a git checkout: %v", err)
	}

	// A change commits + pushes to origin as agentwork/feat-goal-1. The
	// platform-staged project skills (every probed dir: .claude/skills,
	// .opencode/skill, .agents/skills) are infrastructure and must NEVER
	// ride the agent's commit.
	if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, skillDir := range projectSkillsDirs() {
		if err := os.MkdirAll(filepath.Join(wt, skillDir, "demo"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, skillDir, "demo", "SKILL.md"), []byte("---\nname: demo\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sha, err := commitAndPush(ctx, p, wt, repo, branch, p.RunID, "")
	if err != nil {
		t.Fatalf("commit/push: %v", err)
	}
	if sha == "" {
		t.Fatalf("commit/push must report the pushed sha for a change run")
	}
	tree, _ := runGit(ctx, wt, "ls-tree", "-r", "--name-only", "HEAD")
	for _, skillDir := range projectSkillsDirs() {
		if strings.Contains(tree, skillDir) {
			t.Fatalf("staged project skills (%s) must not be committed, tree has:\n%s", skillDir, tree)
		}
	}
	out, err := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1")
	if err != nil || out == "" {
		t.Fatalf("transfer push must land agentwork/feat-goal-1 on origin: %q %v", out, err)
	}
	if !strings.HasPrefix(out, sha) {
		t.Fatalf("origin ref must point at the reported sha: got %q want prefix %q", out, sha)
	}

	// A no-change run pushes nothing (the branch stays where it was).
	before, _ := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1")
	sha2, err := commitAndPush(ctx, p, wt, repo, branch, p.RunID, "")
	if err != nil {
		t.Fatalf("no-change commit: %v", err)
	}
	if sha2 != "" {
		t.Fatalf("a no-change run must report an empty sha, got %q", sha2)
	}
	after, _ := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1")
	if before != after {
		t.Fatalf("no-change run must not push, ref moved %q → %q", before, after)
	}

	// A turn that produces commits WITHOUT staged changes (`agentwork
	// change integrate` merges directly into the worktree) must still
	// publish: the branch tip differs from the transfer ref. Live: the
	// leader's merge commit was dropped, the approved goal's deliver found
	// no branch and failed.
	if out, err := runGit(ctx, wt, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "--allow-empty", "-m", "integrate change"); err != nil {
		t.Fatalf("integrate-style commit: %v: %s", err, out)
	}
	sha3, err := commitAndPush(ctx, p, wt, repo, branch, p.RunID, "")
	if err != nil {
		t.Fatalf("integrate publish: %v", err)
	}
	if sha3 == "" {
		t.Fatalf("a turn with a local merge commit must publish (empty sha reported)")
	}
	out2, err := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1")
	if err != nil || !strings.HasPrefix(out2, sha3) {
		t.Fatalf("transfer ref must advance to the merge commit: %q %v", out2, err)
	}
}

// TestPersistentGoalWorkdir: WRITABLE runs share one workdir per
// (goal, agent) — the worktree outlives the run (session continuity:
// dirt, the branch checkout, and the cwd-keyed CLI session store all
// continue). Read-only roles keep a fresh per-run workdir — their edits
// die with it.
func TestPersistentGoalWorkdir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	url := seedRepo(t)
	p := link.RunDispatchParams{
		RunID: "run-1", GoalID: "goal-1", AgentID: "agent-a",
		DomainID: "dom-1", GitURL: url, DefaultBranch: "master",
		Role: "owner",
	}
	wt1, _, branch1, cleanup1, err := ensureGitWorkdir(ctx, p)
	if err != nil {
		t.Fatalf("workdir: %v", err)
	}
	defer cleanup1()

	// A second run of the same (goal, agent) with a DIFFERENT run id lands
	// on the SAME workdir.
	p2 := p
	p2.RunID = "run-2"
	wt2, _, branch2, cleanup2, err := ensureGitWorkdir(ctx, p2)
	if err != nil {
		t.Fatalf("workdir 2: %v", err)
	}
	defer cleanup2()
	if wt1 != wt2 || branch1 != branch2 {
		t.Fatalf("persistent workdir must be shared: %s vs %s", wt1, wt2)
	}

	// A read-only role gets a fresh per-run dir.
	p3 := p
	p3.RunID = "run-3"
	p3.Role = "review"
	wt3, _, _, cleanup3, err := ensureGitWorkdir(ctx, p3)
	if err != nil {
		t.Fatalf("workdir 3: %v", err)
	}
	defer cleanup3()
	if wt3 == wt1 {
		t.Fatalf("read-only runs must get fresh workdirs, got the owner's %s", wt3)
	}
	if !strings.Contains(wt3, "run-3") {
		t.Fatalf("read-only workdir must be per-run: %s", wt3)
	}
}
