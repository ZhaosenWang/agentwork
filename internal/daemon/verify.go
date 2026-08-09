package daemon

import (
	"context"
	"encoding/json"
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
func commitRunChanges(ctx context.Context, workdir, identity string) error {
	status, err := gitRun(ctx, workdir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return nil // clean — nothing to commit
	}
	// AGENTWORK.md is the daemon-injected coordination guide (per-run content);
	// it must never enter the goal's commits. The pathspec exclude keeps it
	// out of git add -A without touching the repo's .gitignore (the agentwork namespace
	// never touches the user's own AGENTS.md).
	if _, err := gitRun(ctx, workdir, "add", "-A", "--", ".", ":(exclude)AGENTWORK.md"); err != nil {
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

// loadDomainChecks loads the domain's frozen acceptance policy, the verify
// timeout, and the metrics baseline (for coverage_delta guards).
func (d *Daemon) loadDomainChecks(ctx context.Context, domainID string) (service.Checks, int, map[string]float64) {
	var checks service.Checks
	var checksJSON, baselineJSON string
	var timeout int
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT checks, verify_timeout, metrics_baseline FROM domain WHERE id=?`, domainID).
		Scan(&checksJSON, &timeout, &baselineJSON)
	if timeout <= 0 {
		timeout = 600 // DESIGN.v2.md §4: default verify_timeout 10min
	}
	_ = json.Unmarshal([]byte(checksJSON), &checks)
	var baseline map[string]float64
	_ = json.Unmarshal([]byte(baselineJSON), &baseline)
	return checks, timeout, baseline
}

// runVerification executes the domain's verify commands in dir. Each command
// runs under verify_timeout (a hung verify must not keep a run 'running'
// forever — DESIGN.v2.md §4); any non-zero exit fails verification, and a
// failed verification ends the run failed (invariant 14: the goal layer only
// sees 'completed' runs that passed machine verification).
func runVerification(ctx context.Context, dir string, checks service.Checks, timeout int) (string, bool) {
	var report strings.Builder
	for _, cmd := range checks.Verify {
		cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		c := exec.CommandContext(cctx, "sh", "-c", cmd)
		c.Dir = dir
		out, err := c.CombinedOutput()
		cancel()
		report.WriteString("$ " + cmd + "\n")
		report.Write(out)
		if err != nil {
			if cctx.Err() == context.DeadlineExceeded {
				report.WriteString(fmt.Sprintf("\n[verify timed out after %ds]\n", timeout))
			} else {
				report.WriteString("\n[verify failed: " + err.Error() + "]\n")
			}
			return report.String(), false
		}
	}
	return report.String(), true
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

// changedPaths returns the changed paths relative to HEAD, INCLUDING
// untracked files (git diff alone hides untracked files — the agent's work is
// typically uncommitted when guards run; use git status --porcelain).
// globMatch matches a guard pattern against a changed path with the
// intuitive glob semantics humans mean: "*_test.go" matches a test file at
// ANY depth (path.Match's '*' does not cross '/', so "internal/acp/
// parse_test.go" would never match — the guard failed every real run until
// this was fixed). A pattern containing '/' is matched against the full
// path; a bare pattern is matched against the basename and each path segment.
func globMatch(pattern, name string) bool {
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
