package daemon

import (
	"context"
	"strings"

	"github.com/eushing/agentwork/internal/service"
)

// evalGates evaluates the domain's gate rules against the run's diff
// (DESIGN.v2.md §5, M2 rule engine). merge always fires; diff_contains /
// diff_excludes fire on the changed paths; request never fires here — it is
// set directly by the agent's `goal request-approval` call. Returns the
// human-readable fired-gate descriptions, which the daemon records on the
// run row (run.gates_hit): the daemon computes, the goal layer judges.
func evalGates(ctx context.Context, dir, baseSHA string, checks service.Checks) []string {
	names := changedPaths(ctx, dir, baseSHA)
	var hit []string
	// Platform security baseline (DESIGN.v2.md §5.3 — built-in, cannot be
	// overridden by domain policy): DELETING files always demands a human
	// checkpoint. The platform cannot infer intent from a deletion; the
	// delete is judged by a human before anything leaves the branch. (Other
	// baseline items — touching production, spending money — need intent
	// detection and are deferred; deletions are objective via git.)
	if dels := deletedPaths(ctx, dir, baseSHA); len(dels) > 0 {
		hit = append(hit, "platform 安全基线: 删除文件必审（"+strings.Join(dels, ", ")+"）")
	}
	for _, g := range checks.Gates {
		switch g.Name {
		case "merge":
			hit = append(hit, describeGate(g, "每次完成都需人工审批"))
		case "diff_contains":
			for _, name := range names {
				if globMatch(g.Pattern, name) {
					hit = append(hit, describeGate(g, "改动命中 "+g.Pattern))
					break
				}
			}
		case "diff_excludes":
			for _, name := range names {
				if globMatch(g.Pattern, name) {
					hit = append(hit, describeGate(g, "改动触碰禁止路径 "+g.Pattern))
					break
				}
			}
		case "request":
			// handled by GoalService.RequestApproval directly
		}
	}
	return hit
}

// deletedPaths returns the paths DELETED by the run's diff (git diff
// --name-status "D" entries) — the platform security baseline's "删文件必审"
// signal. Measured on the committed diff (baseSHA..HEAD), like changedPaths.
func deletedPaths(ctx context.Context, dir, baseSHA string) []string {
	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA == "" {
		return nil
	}
	out, err := gitRun(ctx, dir, "diff", "--name-status", baseSHA+"..HEAD")
	if err != nil {
		return nil
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(l, "D\t"); ok {
			paths = append(paths, rest)
		}
	}
	return paths
}

func describeGate(g service.GateRule, fallback string) string {
	if g.When != "" {
		return g.Name + ": " + g.When
	}
	return g.Name + ": " + fallback
}

