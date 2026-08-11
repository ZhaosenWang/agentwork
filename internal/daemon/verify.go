package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/service"
)

// commitRunChanges makes the agent's work durable on the goal branch. The
// agent is guided to commit as it works, but the daemon guarantees it: a
// clean tree is a no-op; a dirty tree is committed deterministically (git add
// -A) with the domain's git identity. Deliver merges the branch, so uncommitted
// work would deliver nothing.
//
// What is EXCLUDED from the commit:
//   - AGENTWORK.md unconditionally — the daemon-injected coordination guide
//     (per-run content) is a platform-owned namespace and must never enter
//     the goal's commits (the agentwork namespace never touches the user's
//     own AGENTS.md).
//   - checks.Excludes — the DOMAIN's declared exclusion patterns (glob),
//     compiled by the processor from the repo's own .gitignore / dependency
//     directories and confirmed by the owner. The platform never hardcodes
//     "what a repo should ignore"; the repo's gitignore decisions belong to
//     the repo. (git pathspec excludes are repo-root-relative — no
//     .gitignore-style directory recursion — so patterns like
//     **/node_modules/** are needed for any-depth dirs.)
func commitRunChanges(ctx context.Context, workdir, identity string, excludes []string) error {
	status, err := gitRun(ctx, workdir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil // clean — nothing to commit
	}
	args := []string{"add", "-A", "--", ".", ":(exclude)AGENTWORK.md"}
	for _, e := range excludes {
		args = append(args, ":(exclude)"+e)
	}
	if _, err := gitRun(ctx, workdir, args...); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	// If the only dirty path was AGENTWORK.md (a run that produced no file
	// changes — e.g. a behavior-gate run that just requested approval),
	// nothing is staged and committing would fail with "nothing to commit".
	if _, err := gitRun(ctx, workdir, "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing staged → no-op
	}
	name, email := "agentwork[bot]", "agentwork@local"
	if l, r, ok := strings.Cut(identity, "<"); ok && strings.Contains(r, ">") {
		name = strings.TrimSpace(l)
		email = strings.TrimSuffix(strings.TrimSpace(r), ">")
	}
	if _, err := gitRun(ctx, workdir, "-c", "user.name="+name, "-c", "user.email="+email,
		"commit", "-m", "agentwork: run changes"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	return nil
}

// domainGitIdentity returns the commit identity configured on the domain
// (falling back to the bot identity).
func (d *Daemon) domainGitIdentity(ctx context.Context, domainID string) string {
	var id string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT git_identity FROM domain WHERE id=?`, domainID).Scan(&id)
	return id
}

// loadDomainChecks loads the domain's acceptance policy, the verify timeout,
// and the metrics baseline (for coverage_delta guards). frozen reports
// whether the policy was CONFIRMED by the owner (checks_compiled_at set —
// the "define by the human" guarantee, 决策 2-4/2-5): an unfrozen policy
// must never drive machine verification — nothing runs against it, and the
// goal layer forces the human checkpoint instead.
func (d *Daemon) loadDomainChecks(ctx context.Context, domainID string) (checks service.Checks, timeout int, baseline map[string]float64, frozen bool) {
	var checksJSON, baselineJSON, compiledAt string
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT checks, verify_timeout, metrics_baseline, checks_compiled_at FROM domain WHERE id=?`, domainID).
		Scan(&checksJSON, &timeout, &baselineJSON, &compiledAt)
	if timeout <= 0 {
		timeout = 600 // DESIGN.v2.md §4: default verify_timeout 10min
	}
	_ = json.Unmarshal([]byte(checksJSON), &checks)
	_ = json.Unmarshal([]byte(baselineJSON), &baseline)
	return checks, timeout, baseline, compiledAt != ""
}

// runVerification executes the domain's verification in dir: the setup
// commands (environment preparation — dependency installs, part of the
// acceptance policy) first, then the verify commands. Each command runs under
// verify_timeout (a hung command must not keep a run 'running' forever —
// DESIGN.v2.md §4); any non-zero exit fails verification, and a failed
// verification ends the run failed (invariant 14: the goal layer only sees
// 'completed' runs that passed machine verification). A setup failure is
// attributed as environment preparation, separate from the judgment.
//
// policyIssue reports an OBJECTIVE policy defect via the standard signal:
// POSIX exit code 127 ("command not found") — the shell returns it for a
// missing command AND a missing script path, no text parsing involved. The
// platform flags it so the owner fixes the policy instead of the agent
// burning retries against an impossible check.
func runVerification(ctx context.Context, dir string, checks service.Checks, timeout int) (report string, ok bool, policyIssue bool) {
	var b strings.Builder
	for _, cmd := range checks.Setup {
		b.WriteString("$ " + cmd + "\n")
		out, code := runVerifiedCmd(ctx, dir, cmd, timeout)
		b.WriteString(out)
		if code != 0 {
			b.WriteString(fmt.Sprintf("\n[setup failed (exit %d)]\n", code))
			return b.String(), false, code == 127
		}
	}
	for _, cmd := range checks.Verify {
		b.WriteString("$ " + cmd + "\n")
		out, code := runVerifiedCmd(ctx, dir, cmd, timeout)
		b.WriteString(out)
		if code != 0 {
			b.WriteString(fmt.Sprintf("\n[verify failed (exit %d)]\n", code))
			return b.String(), false, code == 127
		}
	}
	return b.String(), true, false
}

// runSetupOnly runs the acceptance policy's setup commands alone (no verify)
// — the environment-readiness pass at RUN START (决策 3-1: the agent needs
// the same environment it will be judged in). Idempotent by the setup
// commands' own contract; the verification stage re-runs it later. Returns
// ok=false on any setup failure (environment attribution).
func runSetupOnly(ctx context.Context, dir string, checks service.Checks, timeout int) (string, bool) {
	var b strings.Builder
	for _, cmd := range checks.Setup {
		b.WriteString("$ " + cmd + "\n")
		out, code := runVerifiedCmd(ctx, dir, cmd, timeout)
		b.WriteString(out)
		if code != 0 {
			b.WriteString(fmt.Sprintf("\n[setup failed (exit %d)]\n", code))
			return b.String(), false
		}
	}
	return b.String(), true
}

// runVerifiedCmd runs one command under verify_timeout and returns its
// combined output and exit code (0 = ok; -1 = timeout; -2 = could not start).
func runVerifiedCmd(ctx context.Context, dir, cmd string, timeout int) (string, int) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return string(out), -1
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	return string(out), -2
}

var coverRe = regexp.MustCompile(`coverage: ([0-9.]+)% of statements`)

// checkGuards evaluates the structural constraints on the run's diff
// (DESIGN.v2.md §5.1, second form — objective checks that are not commands):
//
//	diff_contains / diff_excludes — glob over the changed paths
//	coverage_delta — coverage reported by a `-cover` verify command vs the
//	                   domain's metrics baseline (skipped with a note when the
//	                   policy has no coverage command or no baseline).
func checkGuards(ctx context.Context, dir, baseSHA string, checks service.Checks, baseline map[string]float64) (string, bool) {
	var report strings.Builder
	names := changedPaths(ctx, dir, baseSHA)
	for _, g := range checks.Guards {
		switch g.Type {
		case "diff_contains", "diff_excludes":
			// An EMPTY diff passes both guards: a run that changed nothing
			// (a behavior-gate run that just requested approval, a consult)
			// has nothing for "all changes must carry a test" to constrain —
			// the claim holds vacuously. Only a diff that EXISTS is judged.
			if len(names) == 0 {
				continue
			}
			matched := false
			for _, name := range names {
				if globMatch(g.Pattern, name) {
					matched = true
					break
				}
			}
			if g.Type == "diff_contains" && !matched {
				report.WriteString("guard diff_contains(" + g.Pattern + "): no changed path matches\n")
				return report.String(), false
			}
			if g.Type == "diff_excludes" && matched {
				report.WriteString("guard diff_excludes(" + g.Pattern + "): a changed path matches\n")
				return report.String(), false
			}
		case "coverage_delta":
			if g.MinDelta <= 0 {
				continue
			}
			base, hasBase := baseline["coverage"]
			coverCmd := ""
			for _, v := range checks.Verify {
				if strings.Contains(v, "cover") {
					coverCmd = v
					break
				}
			}
			if !hasBase || coverCmd == "" {
				report.WriteString("guard coverage_delta: skipped (no baseline or no -cover verify command)\n")
				continue
			}
			out, err := runVerifyOutput(ctx, dir, coverCmd, 600)
			if err != nil {
				report.WriteString("guard coverage_delta: coverage command failed: " + err.Error() + "\n")
				return report.String(), false
			}
			m := coverRe.FindStringSubmatch(out)
			if m == nil {
				report.WriteString("guard coverage_delta: coverage command produced no coverage line\n")
				return report.String(), false
			}
			var cur float64
			_, _ = fmt.Sscanf(m[1], "%f", &cur)
			if cur-base < g.MinDelta {
				report.WriteString(fmt.Sprintf("guard coverage_delta: coverage %.1f%% -> %.1f%% (needed +%.1f%%)\n", base, cur, g.MinDelta))
				return report.String(), false
			}
		}
	}
	return report.String(), true
}

// unattributedDirty lists the worktree's dirty paths that no one can
// account for (DESIGN.v2.md §4, C4): the platform-injected AGENTWORK.md and
// the domain-declared excludes (checks.excludes — dependency dirs the
// platform's own setup materializes) are EXPECTED; everything else is
// returned ('' = clean enough to start a run). Called at run start, BEFORE
// the agent touches anything — a human's manual edits must not be swept
// into the goal's commits.
func unattributedDirty(ctx context.Context, dir string, excludes []string) string {
	// -uall expands untracked DIRECTORIES into individual files: plain
	// porcelain reports "?? web/" for an untracked dir, whose internals the
	// excludes must be matched against (node_modules lives INSIDE web/).
	status, err := gitRun(ctx, dir, "status", "--porcelain", "-uall")
	if err != nil {
		return "" // cannot tell — let the run proceed and surface via commit
	}
	var dirty []string
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// " M path" / "?? path" — take the path part (after the 2-char status).
		path := line
		if len(line) > 3 {
			path = strings.TrimSpace(line[3:])
		}
		if path == "AGENTWORK.md" {
			continue // platform-injected coordination guide
		}
		excluded := false
		for _, e := range excludes {
			if globMatch(e, path) {
				excluded = true
				break
			}
		}
		if !excluded {
			dirty = append(dirty, line)
		}
	}
	return strings.Join(dirty, "\n")
}

// changedPaths returns the changed paths relative to HEAD, INCLUDING
// untracked files (git diff alone hides untracked files — the agent's work is
// typically uncommitted when guards run; use git status --porcelain).
// globMatch matches a guard pattern against a changed path with the
// intuitive glob semantics humans mean: "*_test.go" matches a test file at
// ANY depth (path.Match's '*' does not cross '/', so "internal/acp/
// parse_test.go" would never match — the guard failed every real run until
// this was fixed). A bare pattern matches the basename and each path segment;
// a pattern containing '/' matches the full path; "**" crosses any depth
// (doublestar — path.Match treats it as two plain '*', which is wrong for
// the "**/*_test.go" patterns processor agents produce).
func globMatch(pattern, name string) bool {
	if strings.Contains(pattern, "**") {
		// "**" crosses directories; "**/" is optional at depth zero
		// ("**/*_test.go" matches "x_test.go" and "a/b/x_test.go").
		re, err := regexp.Compile(globToRegex(pattern))
		if err != nil {
			return false
		}
		return re.MatchString(name)
	}
	if strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, name)
		return ok
	}
	for _, seg := range strings.Split(name, "/") {
		if ok, _ := path.Match(pattern, seg); ok {
			return true
		}
	}
	return false
}

// globToRegex translates a glob pattern into an anchored regex with doublestar
// semantics: '*' matches within a segment, '**' crosses segments, '**/' is
// optional (zero or more directories).
func globToRegex(p string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					b.WriteString("/?")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	b.WriteString("$")
	return b.String()
}

// changedPaths returns the paths this run changed: baseSHA..HEAD. The run's
// work is committed by the time guards run (the agent commits itself and the
// daemon commits leftover work), so a working-tree diff would be empty — the
// diff is measured against the run's start commit (the run's baseline).
func changedPaths(ctx context.Context, dir, baseSHA string) []string {
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		return nil
	}
	out, err := gitRun(ctx, dir, "diff", "--name-only", baseSHA+"..HEAD")
	if err != nil {
		return nil
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths
}

func runVerifyOutput(ctx context.Context, dir, cmd string, timeout int) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

// buildEvidence assembles the checkpoint evidence bundle (DESIGN.v2.md §4,
// decision 2-3): diff stats + changed paths + verify output + agent summary.
// Stored on run.evidence and shown on the approval card.
func buildEvidence(ctx context.Context, dir, baseSHA, agentSummary, verifyReport, guardReport string) string {
	stats := ""
	if baseSHA != "" {
		stats, _ = gitRun(ctx, dir, "diff", "--stat", baseSHA+"..HEAD")
	}
	ev := map[string]any{
		"diff_stat":   strings.TrimSpace(stats),
		"changed":     changedPaths(ctx, dir, baseSHA),
		"verify":      verifyReport,
		"guards":      guardReport,
		"agent":       agentSummary,
	}
	b, _ := json.Marshal(ev)
	return string(b)
}
