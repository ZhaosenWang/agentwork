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

// TestFireScheduleAtomicBirth: the fired goal, its schedule_run row and its
// FIRST run are born in one transaction (P0-3) — and a duplicate firing of
// the same planned_at is a no-op (uq idempotency): one goal, one run.
func TestFireScheduleAtomicBirth(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	bus := events.NewBus()

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
	sched, err := service.NewScheduleService(st, bus).Create(ctx, service.Schedule{
		Name: "s", TitleTemplate: "t", Description: "d",
		AssigneeType: "agent", AssigneeID: agent.ID, DomainID: domain.ID,
		CronExpression: "*/1 * * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc}

	due := scheduleDueRow{
		ScheduleID: sched.ID, TitleTemplate: "t", Description: "d",
		AssigneeType: "agent", AssigneeID: agent.ID, DomainID: domain.ID,
		CronExpression: "*/1 * * * *", Timezone: "UTC",
		NextRunAt: "2026-08-13T12:00:00Z",
	}
	d.fireSchedule(ctx, due)

	// One goal, one schedule_run, one queued owner run — all born together.
	var goals, firings, runs int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE created_by_id=?`, sched.ID).Scan(&goals); err != nil || goals != 1 {
		t.Fatalf("exactly 1 fired goal, got %d (err %v)", goals, err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schedule_run WHERE schedule_id=?`, sched.ID).Scan(&firings); err != nil || firings != 1 {
		t.Fatalf("exactly 1 firing, got %d (err %v)", firings, err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id IN (SELECT goal_id FROM schedule_run WHERE schedule_id=?) AND status='queued'`,
		sched.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("exactly 1 queued first run, got %d (err %v)", runs, err)
	}

	// A duplicate firing of the same planned_at (concurrent tick) is a no-op.
	d.fireSchedule(ctx, due)
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE created_by_id=?`, sched.ID).Scan(&goals); err != nil || goals != 1 {
		t.Fatalf("duplicate firing must not create a second goal, got %d (err %v)", goals, err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id IN (SELECT goal_id FROM schedule_run WHERE schedule_id=?)`,
		sched.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("duplicate firing must not enqueue a second run, got %d (err %v)", runs, err)
	}
}
