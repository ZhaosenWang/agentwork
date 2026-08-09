package daemon

import (
	"context"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// TestFireScheduleGoalInsert locks the fireSchedule goal INSERT: the fired
// goal must carry assignee_type/assignee_id from the schedule, status
// 'active' (so the dispatch chain picks it up), domain_id, and the schedule
// as created_by_id. This regressed twice (column/value misalignment wrote
// 'active' into assignee_id and left status empty — fired goals were never
// scheduled).
func TestFireScheduleGoalInsert(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	bus := events.NewBus()

	// Seed: runtime + agent + domain (mirrors the m0/m1 scripts).
	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	agent, err := service.NewAgentService(st, bus).Create(ctx, service.Agent{Name: "maintainer", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	domain, err := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "d", GitURL: "https://example.com/d.git"})
	if err != nil {
		t.Fatalf("domain: %v", err)
	}

	// fireSchedule's own goal-insert function (regression: the column/value
	// mapping was copied into tests before and drifted twice).
	goalID := "test-goal-id"
	ts := "2026-08-09T00:00:00Z"
	scheduleID := "test-schedule-id"
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := insertFiredGoal(ctx, tx, goalID, "t", "d", domain.ID, "agent", agent.ID, scheduleID, ts); err != nil {
		t.Fatalf("insertFiredGoal: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The fired goal must be dispatchable: correct assignee + active status.
	g, err := service.NewGoalService(st, bus).Get(ctx, goalID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g.Status != "active" {
		t.Fatalf("expected status active, got %q (dispatch never picks it up)", g.Status)
	}
	if g.AssigneeType != "agent" || g.AssigneeID != agent.ID {
		t.Fatalf("assignee mismatch: %s/%s", g.AssigneeType, g.AssigneeID)
	}
	if g.DomainID != domain.ID {
		t.Fatalf("domain mismatch: %q", g.DomainID)
	}
	if g.CreatedByID != scheduleID {
		t.Fatalf("created_by_id should be the schedule id, got %q", g.CreatedByID)
	}

	// And EnqueueForGoal must produce a run (the full dispatch entry).
	rs := service.NewRunService(st, bus)
	gs := service.NewGoalService(st, bus)
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	run, err := rs.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatalf("enqueue for fired goal: %v", err)
	}
	if run == nil || run.Status != "queued" {
		t.Fatalf("expected a queued run, got %+v", run)
	}
	if run.AgentID != agent.ID {
		t.Fatalf("run agent mismatch: %q", run.AgentID)
	}
}
