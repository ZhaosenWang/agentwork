package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

	// A change commits + pushes to origin as agentwork/feat-goal-1.
	if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAndPush(ctx, p, wt, repo, branch, p.RunID, ""); err != nil {
		t.Fatalf("commit/push: %v", err)
	}
	if out, err := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1"); err != nil || out == "" {
		t.Fatalf("transfer push must land agentwork/feat-goal-1 on origin: %q %v", out, err)
	}

	// A no-change run pushes nothing (the branch stays where it was).
	before, _ := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1")
	if err := commitAndPush(ctx, p, wt, repo, branch, p.RunID, ""); err != nil {
		t.Fatalf("no-change commit: %v", err)
	}
	after, _ := runGit(ctx, repo, "ls-remote", "origin", "refs/heads/agentwork/feat-goal-1")
	if before != after {
		t.Fatalf("no-change run must not push, ref moved %q → %q", before, after)
	}
}
