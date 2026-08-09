package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/service"
)

// newTestRepo initializes a local git repository with one commit on main.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@local")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	return dir
}

// TestCommitRunChanges: a dirty tree is committed deterministically; a clean
// tree is a no-op.
func TestCommitRunChanges(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()

	// Clean tree → no-op.
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>"); err != nil {
		t.Fatalf("clean tree must be a no-op: %v", err)
	}

	// Dirty tree → committed with the given identity.
	if err := os.WriteFile(filepath.Join(dir, "new_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>"); err != nil {
		t.Fatalf("commit dirty tree: %v", err)
	}
	author, err := gitRun(ctx, dir, "log", "-1", "--format=%an <%ae>")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(author) != "agentwork[bot] <aw@local>" {
		t.Fatalf("commit author mismatch: %q", author)
	}
	// Tree is now clean again.
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("second commit must be a no-op: %v", err)
	}
}

// TestCommitRunChangesOnlyGuide: a run whose only dirty path is the
// daemon-injected AGENTWORK.md (e.g. a behavior-gate run that just requested
// approval) must be a NO-OP — the exclude leaves nothing staged and a commit
// would fail with "nothing to commit" (regression from the pathspec exclude).
func TestCommitRunChangesOnlyGuide(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "AGENTWORK.md"), []byte("coordination guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("AGENTWORK.md-only dirty tree must be a no-op: %v", err)
	}
	// Still clean: nothing was committed.
	status, _ := gitRun(ctx, dir, "status", "--porcelain")
	if strings.TrimSpace(status) != "?? AGENTWORK.md" {
		t.Fatalf("expected only AGENTWORK.md untracked, got: %s", status)
	}
}

// TestCheckGuardsEmptyDiffPasses: a run with an EMPTY diff (a behavior-gate
// run that just requested approval) must pass diff_contains — "all changes
// carry a test" holds vacuously when there are no changes.
func TestCheckGuardsEmptyDiffPasses(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()
	base, _ := gitRun(ctx, dir, "rev-parse", "HEAD")
	report, ok := checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_contains", Pattern: "*_test.go"}},
	}, nil)
	if !ok {
		t.Fatalf("empty diff must pass diff_contains, report:\n%s", report)
	}
}

// TestRunVerification: exit 0 passes, non-zero fails, and the report carries
// the command output.
func TestRunVerification(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()

	report, ok := runVerification(ctx, dir, service.Checks{
		Verify: []string{"test -f main.go", "echo hello"},
	}, 30)
	if !ok {
		t.Fatalf("expected pass, report:\n%s", report)
	}
	if !strings.Contains(report, "hello") {
		t.Fatalf("report should include command output:\n%s", report)
	}

	report, ok = runVerification(ctx, dir, service.Checks{
		Verify: []string{"test -f missing.go"},
	}, 30)
	if ok {
		t.Fatalf("expected fail, report:\n%s", report)
	}
	if !strings.Contains(report, "exit") {
		t.Fatalf("report should mention the failure:\n%s", report)
	}
}

// TestCheckGuardsDiffContains: the guard matches the run's committed changes
// (baseSHA..HEAD) against the glob — the daemon commits the agent's work
// before guards run, so the worktree is clean and the diff is measured from
// the run's baseline commit (regression: this was git status before, which
// was always empty post-commit and failed every diff_contains guard).
func TestCheckGuardsDiffContains(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()
	base, _ := gitRun(ctx, dir, "rev-parse", "HEAD")

	// Change only main.go (committed) → diff_contains(*_test.go) fails.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	report, ok := checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_contains", Pattern: "*_test.go"}},
	}, nil)
	if ok {
		t.Fatalf("expected guard failure, report:\n%s", report)
	}

	// Add a test file (committed) → guard passes.
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	report, ok = checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_contains", Pattern: "*_test.go"}},
	}, nil)
	if !ok {
		t.Fatalf("expected guard pass, report:\n%s", report)
	}

	// A test file in a NESTED directory must also match the bare pattern —
	// this is the regression that failed every real run: path.Match's '*'
	// does not cross '/', so "internal/acp/parse_test.go" never matched
	// "*_test.go".
	if err := os.MkdirAll(filepath.Join(dir, "internal", "acp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "acp", "parse_test.go"), []byte("package acp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	report, ok = checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_contains", Pattern: "*_test.go"}},
	}, nil)
	if !ok {
		t.Fatalf("nested *_test.go must match bare pattern, report:\n%s", report)
	}

	// diff_excludes: forbidden path untouched → pass.
	report, ok = checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_excludes", Pattern: "config/*"}},
	}, nil)
	if !ok {
		t.Fatalf("expected pass when forbidden path untouched, report:\n%s", report)
	}
	// Touch the forbidden path (committed) → fails.
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "prod.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	report, ok = checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_excludes", Pattern: "config/*"}},
	}, nil)
	if ok {
		t.Fatalf("expected guard failure for forbidden path, report:\n%s", report)
	}
}

// TestBuildEvidence: the bundle carries changed paths, verify output, and the
// agent summary — the approval card's raw material (decision 2-3).
func TestBuildEvidence(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()
	base, _ := gitRun(ctx, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev := buildEvidence(ctx, dir, base, "agent summary", "$ go test\nok", "")
	if !strings.Contains(ev, "x_test.go") {
		t.Fatalf("evidence must list changed paths: %s", ev)
	}
	if !strings.Contains(ev, "agent summary") {
		t.Fatalf("evidence must carry the agent summary: %s", ev)
	}
}
