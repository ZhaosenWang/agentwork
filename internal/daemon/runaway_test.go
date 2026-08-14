package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// TestReapRunawayRun: a run whose process outlived its budget (the
// cancellation chain broke) is terminalized by the DB-level reaper — the
// owner single-flight releases regardless of whether the process ever dies
// (P1, 决策 6-15⑦). The stamp is conditional and carries reason='runaway';
// the goal stays active for the human (决策 2-6 semantics).
func TestReapRunawayRun(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	d := &Daemon{
		st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc,
		runCancels:       make(map[string]context.CancelFunc),
		runCancelReasons: make(map[string]string),
		ctx:              context.Background(),
	}

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentSvc := service.NewAgentService(st, bus)
	a, err := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent a: %v", err)
	}
	agentA := a.ID
	ds := service.NewDomainService(st, bus)
	dom, err := ds.Create(ctx, service.Domain{Name: "d", GitURL: "https://example.com/x.git"})
	if err != nil {
		t.Fatal(err)
	}
	// A tiny budget so the reaper fires without real waiting.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE domain SET max_run_duration=1 WHERE id=?`, dom.ID); err != nil {
		t.Fatal(err)
	}
	g, err := goalSvc.Create(ctx, service.Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: dom.ID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runSvc.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatal(err)
	}
	// The run is "running" and started long ago (a hung process).
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE id=?`, old, run.ID); err != nil {
		t.Fatal(err)
	}

	d.reapRunawayRuns(ctx)

	var status, reason string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status, cancel_reason FROM run WHERE id=?`, run.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || reason != "runaway" {
		t.Fatalf("the reaper must terminalize the runaway run, got %s/%s", status, reason)
	}
	// The goal stays active — the human decides (决策 2-6).
	after, _ := goalSvc.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("a reaped run leaves the goal active, got %q", after.Status)
	}
	// A LATE result from the zombie process cannot resurrect the stamp.
	err = runSvc.Finish(ctx, run.ID, "completed", "I actually finished")
	if err != service.ErrRunAlreadyTerminal {
		t.Fatalf("late result must be dropped, got %v", err)
	}
	// A fresh run whose started_at is recent must NOT be reaped (the grace).
	run2, err := runSvc.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), run2.ID); err != nil {
		t.Fatal(err)
	}
	d.reapRunawayRuns(ctx)
	var status2 string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM run WHERE id=?`, run2.ID).Scan(&status2); err != nil {
		t.Fatal(err)
	}
	if status2 != "running" {
		t.Fatalf("a fresh run must survive the reaper, got %q", status2)
	}
}
