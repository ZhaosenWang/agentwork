package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestParallelConsultWorktreeDetaches: a consult run claimed WHILE the owner
// run holds the goal branch's checkout must get its own DETACHED snapshot —
// git allows one checkout per branch, and the old owner-path fallback made
// the parallel consult fail with "a branch named … already exists" (live:
// the self-introduction goal's consult to opencode). Review runs share the
// path (read-only participants, 决策 6-2/6-7).
func TestParallelConsultWorktreeDetaches(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate runsRoot() from the real daemon
	ctx := context.Background()

	// Source repo with one commit on main.
	src := filepath.Join(t.TempDir(), "src")
	mustRunGit(t, "", "init", "-b", "main", src)
	mustRunGit(t, src, "config", "user.email", "t@t")
	mustRunGit(t, src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, src, "add", ".")
	mustRunGit(t, src, "commit", "-m", "init")

	domainID := "test-domain"
	goalID := "test-goal"
	d := &Daemon{}

	// The owner claims the goal branch (its only checkout).
	ownerWt, err := d.ensureRunWorktreeFor(ctx, "run-owner", domainID, goalID, "", "owner", "file://"+src, "", "main")
	if err != nil {
		t.Fatalf("owner worktree: %v", err)
	}

	// A parallel consult must succeed on a detached snapshot — not fail on
	// the branch already being checked out.
	consultWt, err := d.ensureRunWorktreeFor(ctx, "run-consult", domainID, goalID, "", "consult", "file://"+src, "", "main")
	if err != nil {
		t.Fatalf("parallel consult worktree: %v", err)
	}
	out, err := exec.Command("git", "-C", consultWt, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil || string(out) != "HEAD\n" {
		t.Fatalf("consult worktree must be detached, got %q (err %v)", out, err)
	}
	// The owner's checkout is untouched.
	out2, err := exec.Command("git", "-C", ownerWt, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil || string(out2) != goalBranchName(goalID)+"\n" {
		t.Fatalf("owner worktree must still hold the branch, got %q (err %v)", out2, err)
	}
	// A review run shares the read-only path.
	reviewWt, err := d.ensureRunWorktreeFor(ctx, "run-review", domainID, goalID, "", "review", "file://"+src, "", "main")
	if err != nil {
		t.Fatalf("parallel review worktree: %v", err)
	}
	out3, err := exec.Command("git", "-C", reviewWt, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil || string(out3) != "HEAD\n" {
		t.Fatalf("review worktree must be detached, got %q (err %v)", out3, err)
	}
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
