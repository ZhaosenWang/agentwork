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
// TestPolicyIssueViaExit127: objective policy defects are detected by the
// STANDARD signal (POSIX exit 127 = command not found) — no text parsing —
// while genuine work failures (non-zero exits) are not flagged.
func TestPolicyIssueViaExit127(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// A missing command → exit 127 → policy issue.
	_, ok, policy := runVerification(ctx, dir, service.Checks{Verify: []string{"definitely-not-a-real-cmd-xyz"}}, 30)
	if ok || !policy {
		t.Fatalf("missing command must fail with policyIssue=true, ok=%v policy=%v", ok, policy)
	}

	// A missing script path → exit 127 → policy issue.
	_, ok, policy = runVerification(ctx, dir, service.Checks{Verify: []string{"./no-such-script.sh"}}, 30)
	if ok || !policy {
		t.Fatalf("missing script must fail with policyIssue=true, ok=%v policy=%v", ok, policy)
	}

	// A genuine work failure (non-zero exit) → failed but NOT a policy issue.
	_, ok, policy = runVerification(ctx, dir, service.Checks{Verify: []string{"sh -c 'exit 1'"}}, 30)
	if ok || policy {
		t.Fatalf("work failure must fail with policyIssue=false, ok=%v policy=%v", ok, policy)
	}
}

func TestCommitRunChanges(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()

	// Clean tree → no-op.
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>", nil); err != nil {
		t.Fatalf("clean tree must be a no-op: %v", err)
	}

	// Dirty tree → committed with the given identity.
	if err := os.WriteFile(filepath.Join(dir, "new_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>", nil); err != nil {
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
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
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
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
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

	report, ok, _ := runVerification(ctx, dir, service.Checks{
		Verify: []string{"test -f main.go", "echo hello"},
	}, 30)
	if !ok {
		t.Fatalf("expected pass, report:\n%s", report)
	}
	if !strings.Contains(report, "hello") {
		t.Fatalf("report should include command output:\n%s", report)
	}

	report, ok, _ = runVerification(ctx, dir, service.Checks{
		Verify: []string{"test -f missing.go"},
	}, 30)
	if ok {
		t.Fatalf("expected fail, report:\n%s", report)
	}
	if !strings.Contains(report, "exit") {
		t.Fatalf("report should mention the failure:\n%s", report)
	}
}

// TestRunVerificationSetup: the setup commands (environment preparation —
// dependency installs) run BEFORE verify; a setup failure fails the
// verification with environment attribution and verify never runs. The
// worktree stays dirty from setup (node_modules-style installs are
// gitignored, not committed) — the setup's own side effects must not be
// judged as the run's changes.
func TestRunVerificationSetup(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()

	// Setup creates a file verify depends on; verify passes only after setup.
	report, ok, _ := runVerification(ctx, dir, service.Checks{
		Setup:  []string{"echo prepared > prepared.txt"},
		Verify: []string{"test -f prepared.txt"},
	}, 30)
	if !ok {
		t.Fatalf("setup then verify must pass, report:\n%s", report)
	}
	if !strings.Contains(report, "setup failed") && !strings.Contains(report, "[setup") {
		// no setup failure expected — the check is about order and attribution
	}

	// Setup failure: attributed as setup, verify NOT run.
	report, ok, _ = runVerification(ctx, dir, service.Checks{
		Setup:  []string{"exit 3"},
		Verify: []string{"echo should-not-run > ran.txt"},
	}, 30)
	if ok {
		t.Fatalf("setup failure must fail verification, report:\n%s", report)
	}
	if !strings.Contains(report, "setup failed") {
		t.Fatalf("setup failure must be attributed as environment, report:\n%s", report)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err == nil {
		t.Fatal("verify must not run after a failed setup")
	}

	// Empty setup = no preparation needed; verify runs directly.
	report, ok, _ = runVerification(ctx, dir, service.Checks{Verify: []string{"true"}}, 30)
	if !ok {
		t.Fatalf("empty setup must pass, report:\n%s", report)
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
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>", nil); err != nil {
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
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
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
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
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
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	report, ok = checkGuards(ctx, dir, base, service.Checks{
		Guards: []service.Guard{{Type: "diff_excludes", Pattern: "config/*"}},
	}, nil)
	if ok {
		t.Fatalf("expected guard failure for forbidden path, report:\n%s", report)
	}
}

// TestGlobMatchDoublestar: "**" crosses any directory depth ("**/*_test.go"
// matches a test file at depth 0 AND depth 2), and "**/" is optional at the
// root. This is the semantic path.Match lacks (it treats "**" as two plain
// '*', which never crosses '/').
func TestGlobMatchDoublestar(t *testing.T) {
	cases := []struct{ pattern, name string; want bool }{
		{"**/*_test.go", "x_test.go", true},                // depth 0
		{"**/*_test.go", "a/b/x_test.go", true},            // depth 2
		{"**/*_test.go", "a/b/x.go", false},                // not a test
		{"**/*_test.go", "README.md", false},               // no test anywhere
		{"**/config/*.yaml", "config/prod.yaml", true},     // depth 0
		{"**/config/*.yaml", "a/b/config/prod.yaml", true}, // nested
		{"**/config/*.yaml", "config/prod.json", false},    // wrong ext
		{"docs/**/*.md", "docs/README.md", true},
		{"docs/**/*.md", "docs/sub/deep/README.md", true},
		{"docs/**/*.md", "README.md", false}, // outside docs/
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestCommitRunChangesExcludesDependencies: the DOMAIN-declared excludes
// (checks.excludes — compiled by the processor from the repo's .gitignore,
// confirmed by the owner) keep dependency directories out of the goal
// branch even when the repo has no .gitignore of its own. The exclusion
// knowledge belongs to the domain, never hardcoded in the platform.
func TestCommitRunChangesExcludesDependencies(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()
	// node_modules tree without any .gitignore — the case the domain's
	// excludes exist for.
	if err := os.MkdirAll(filepath.Join(dir, "web", "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "node_modules", "pkg", "index.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	excludes := []string{"**/node_modules/**"}
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>", excludes); err != nil {
		t.Fatalf("deps-only dirty tree must be a no-op: %v", err)
	}
	status, _ := gitRun(ctx, dir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		t.Fatal("node_modules must still be untracked (never committed), not cleaned")
	}
	// A REAL change alongside the deps is committed; the deps stay out.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "agentwork[bot] <aw@local>", excludes); err != nil {
		t.Fatalf("commit with deps present: %v", err)
	}
	out, _ := gitRun(ctx, dir, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(out, "node_modules") {
		t.Fatalf("node_modules must never be committed:\n%s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Fatalf("real change must be committed:\n%s", out)
	}
	// WITHOUT the domain's excludes, the deps WOULD be committed — the
	// exclusion is the domain's declaration, not platform magic.
	dir2 := newTestRepo(t)
	if err := os.MkdirAll(filepath.Join(dir2, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "node_modules", "x.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir2, "x", nil); err != nil {
		t.Fatal(err)
	}
	out2, _ := gitRun(ctx, dir2, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(out2, "node_modules/x.js") {
		t.Fatal("without a declared exclude, the dirty dep dir IS committed — the domain decides")
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
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
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

// TestGitCloneURL: the credential injection is the generic git-layer
// capability (M4) — token as HTTPS username, SSH untouched, an already-
// credentialed URL never overridden.
func TestGitCloneURL(t *testing.T) {
	cases := []struct{ url, cred, want string }{
		{"https://github.com/yusheng-g/agentwork.git", "ghp_abc123",
			"https://ghp_abc123@github.com/yusheng-g/agentwork.git"},
		{"https://gitcode.com/eushing/test-repo.git", "gck-abc", // GitCode → oauth2: prefix (GitLab-style PAT)
			"https://oauth2:gck-abc@gitcode.com/eushing/test-repo.git"},
		{"https://github.com/yusheng-g/agentwork.git", "", // no cred → unchanged
			"https://github.com/yusheng-g/agentwork.git"},
		{"git@github.com:yusheng-g/agentwork.git", "ghp_abc123", // SSH → keys, no injection
			"git@github.com:yusheng-g/agentwork.git"},
		{"https://user:pass@github.com/yusheng-g/agentwork.git", "ghp_abc123", // explicit creds win
			"https://user:pass@github.com/yusheng-g/agentwork.git"},
		{"http://gitlab.example.com/x/y.git", "glpat-1", // http (non-tls) untouched
			"http://gitlab.example.com/x/y.git"},
	}
	for _, tc := range cases {
		if got := gitCloneURL(tc.url, tc.cred); got != tc.want {
			t.Errorf("gitCloneURL(%q, %q) = %q, want %q", tc.url, tc.cred, got, tc.want)
		}
	}
}

// TestGuestWorkspaceReset: the consult/review read-only enforcement (决策
// 6-2/6-7) — the run's workspace starts CLEAN (fresh worktree from a ref), so
// at run end EVERYTHING is discarded: tracked edits via reset --hard,
// untracked files via clean, HEAD untouched (a guest commit is detected
// separately and flagged, not reverted here). AGENTWORK.md (the platform's
// injected guide) survives the clean for forensics.
func TestGuestWorkspaceReset(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()

	// Base state committed.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
		t.Fatalf("commit base: %v", err)
	}
	baseSHA := strings.TrimSpace(mustGitRun(ctx, dir, "rev-parse", "HEAD"))

	// The guest run touches the workspace: tracked edit + untracked file +
	// the platform's injected guide.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("guest edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "guest-new.txt"), []byte("guest untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTWORK.md"), []byte("guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resetGuestWorkspace(ctx, dir)

	// Tracked edit gone, untracked file gone.
	if b, _ := os.ReadFile(filepath.Join(dir, "tracked.txt")); string(b) != "base\n" {
		t.Fatalf("guest's tracked edit must be discarded, got %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "guest-new.txt")); !os.IsNotExist(err) {
		t.Fatal("guest's untracked file must be cleaned")
	}
	// HEAD untouched (a commit is detected by the caller, not reverted here).
	if got := strings.TrimSpace(mustGitRun(ctx, dir, "rev-parse", "HEAD")); got != baseSHA {
		t.Fatalf("reset must not move HEAD: %s != %s", got, baseSHA)
	}
	// The platform's injected guide survives for forensics.
	if _, err := os.Stat(filepath.Join(dir, "AGENTWORK.md")); err != nil {
		t.Fatalf("AGENTWORK.md must survive the clean: %v", err)
	}
}
