package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// seedLocalRepo creates a local bare repo with a commit on branch "master"
// (the default the tests target) and returns its file:// URL.
func seedLocalRepo(t *testing.T) string {
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
	run(work, "symbolic-ref", "HEAD", "refs/heads/master") // default branch = master
	run(work, "remote", "add", "origin", bare)
	// touch + commit so the repo has history
	if err := exec.Command("sh", "-c", "echo hi > "+filepath.Join(work, "f.txt")).Run(); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "f.txt")
	run(work, "commit", "-m", "init")
	run(work, "push", "origin", "master")
	return "file://" + bare
}

// TestDomainGitCheck covers the config-time git probe (决策 6-24): the repo
// URL, the branch, and the read permission are verified BEFORE the first
// run, over a local file:// repo (no network, no credentials needed — the
// token embedding path is shared with the clone and covered elsewhere).
func TestDomainGitCheck(t *testing.T) {
	d := &Daemon{}
	ctx := context.Background()
	url := seedLocalRepo(t)

	// Reachable repo, branch exists (explicit).
	res := d.TestDomainGit(ctx, url, "master", "")
	if !res.OK || !res.BranchExists || res.ResolvedBranch != "master" {
		t.Fatalf("expected ok+master, got %+v", res)
	}
	if len(res.Refs) != 1 || res.Refs[0] != "master" {
		t.Fatalf("expected refs [master], got %v", res.Refs)
	}

	// Wrong branch: repo reachable but the configured branch is missing.
	res = d.TestDomainGit(ctx, url, "main", "")
	if !res.OK || res.BranchExists {
		t.Fatalf("expected ok + missing branch, got %+v", res)
	}

	// Branch left blank: resolve the remote's actual HEAD (master here).
	res = d.TestDomainGit(ctx, url, "", "")
	if !res.OK || !res.BranchExists || res.ResolvedBranch != "master" {
		t.Fatalf("expected resolved master, got %+v", res)
	}

	// Unreachable repo: the error survives (sanitized), ok=false.
	res = d.TestDomainGit(ctx, "file:///no/such/repo.git", "main", "")
	if res.OK || res.Error == "" {
		t.Fatalf("expected failure with error, got %+v", res)
	}

	// Empty URL: validation error.
	res = d.TestDomainGit(ctx, "", "main", "")
	if res.OK || res.Error == "" {
		t.Fatalf("expected validation error, got %+v", res)
	}
}
