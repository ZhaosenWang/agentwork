package daemon

import (
	"context"

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

func describeGate(g service.GateRule, fallback string) string {
	if g.When != "" {
		return g.Name + ": " + g.When
	}
	return g.Name + ": " + fallback
}

