package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// TestHandoffCancelsOldOwnerRun: a goal changing hands (human reassign or an
// agent's `goal assign`) must terminate the previous owner's running run.
// Without it the handed-off agent's run keeps running (it believes its turn
// is over — the handoff was the point of its turn), and the new owner's run
// waits queued forever behind per-goal serialization: a deadlock. The cancel
// targets only runs of agents that are no longer the owner; a re-assign to
// the same agent must NOT cut its own live run.
func TestHandoffCancelsOldOwnerRun(t *testing.T) {
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
		ctx:              context.Background(), // handlers use d.ctx, not the event ctx
	}
	// The bus runs handlers on its own goroutines with no acknowledgement —
	// wrap the handler with a done channel so the test waits for it (the
	// store would be closed before the handler ran otherwise).
	handled := make(chan struct{})
	bus.Subscribe("goal:assigned", func(ctx context.Context, e events.Event) {
		d.onGoalAssigned(ctx, e)
		close(handled)
	})
	waitHandled := func() { <-handled }

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentSvc := service.NewAgentService(st, bus)
	agentA, err := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent a: %v", err)
	}
	agentB, err := agentSvc.Create(ctx, service.Agent{Name: "b", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent b: %v", err)
	}
	dom, err := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}

	// A's running run (registered cancel) + B's queued run.
	insertRun := func(id, agentID, status string) {
		// All-placeholder form — mixing a literal into VALUES shifts modernc's
		// positional binding (agent_id would receive the status value and fail
		// the FK check).
		_, err := st.DB().ExecContext(ctx,
			`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, goal.ID, agentID, "worker", status, 1, nowStr(), nowStr())
		if err != nil {
			t.Fatalf("insert run %s: %v", id, err)
		}
	}
	insertRun("run-a", agentA.ID, "running")
	insertRun("run-b", agentB.ID, "queued")

	// Simulate runTask's registration for the running run.
	var mu sync.Mutex
	cancelled := map[string]bool{}
	mark := func(id string) context.CancelFunc {
		return func() {
			mu.Lock()
			cancelled[id] = true
			mu.Unlock()
		}
	}
	d.mu.Lock()
	d.runCancels["run-a"] = mark("run-a")
	d.runCancels["run-b"] = mark("run-b") // registered defensively; must NOT fire
	d.mu.Unlock()

	// Hand off A → B (Assign publishes goal:assigned; the daemon cuts A's run).
	if _, err := goalSvc.Assign(ctx, goal.ID, "agent", agentB.ID, "take over", "", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	waitHandled()
	mu.Lock()
	defer mu.Unlock()
	if !cancelled["run-a"] {
		t.Fatalf("old owner's running run was not cancelled after handoff")
	}
	if cancelled["run-b"] {
		t.Fatalf("new owner's run must keep running (it owns the goal now)")
	}
}

// TestHandoffToHumanCancelsAllRuns: handing the goal back to a human means no
// agent may keep running on it — every running run is cut.
func TestHandoffToHumanCancelsAllRuns(t *testing.T) {
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
		ctx:              context.Background(), // handlers use d.ctx, not the event ctx
	}
	handled := make(chan struct{})
	bus.Subscribe("goal:assigned", func(ctx context.Context, e events.Event) {
		d.onGoalAssigned(ctx, e)
		close(handled)
	})

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentA, err := service.NewAgentService(st, bus).Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	dom, err := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		"run-a", goal.ID, agentA.ID, "worker", "running", 1, nowStr(), nowStr()); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	var fired bool
	d.mu.Lock()
	d.runCancels["run-a"] = func() { fired = true }
	d.mu.Unlock()

	if _, err := goalSvc.Assign(ctx, goal.ID, "human", "", "", "", ""); err != nil {
		t.Fatalf("assign to human: %v", err)
	}
	<-handled
	if !fired {
		t.Fatalf("running run must be cut when the goal returns to human")
	}
}


// TestCancelStopsRunningRun: cancelling a goal (决策 4-12) terminates its
// still-running run — a cancelled goal must not keep an agent burning
// compute. The stop reuses the runCancels registry with reason "stopped".
func TestCancelStopsRunningRun(t *testing.T) {
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
	handled := make(chan struct{})
	bus.Subscribe("goal:finished", func(ctx context.Context, e events.Event) {
		d.onGoalFinished(ctx, e)
		close(handled)
	})

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentA, err := service.NewAgentService(st, bus).Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	dom, err := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		"run-a", goal.ID, agentA.ID, "worker", "running", 1, nowStr(), nowStr()); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	var fired bool
	d.mu.Lock()
	d.runCancels["run-a"] = func() { fired = true }
	d.mu.Unlock()

	if _, err := goalSvc.Cancel(ctx, goal.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	<-handled
	if !fired {
		t.Fatalf("running run must be stopped when the goal is cancelled")
	}
}

// TestStopRunRejectsForeignRun: StopRun refuses a run that does not belong
// to the given goal.
func TestStopRunRejectsForeignRun(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{
		st:         st,
		runCancels: make(map[string]context.CancelFunc),
		ctx:        context.Background(),
	}
	// No run rows at all: any stop attempt must fail, not silently no-op.
	if err := d.StopRun("goal-x", "run-y"); err == nil {
		t.Fatal("StopRun for a nonexistent run: want error")
	}
}

// TestHandoffWindowStampMarksHandedOff: a handoff landing in the
// claim→register window (run status='running' but no registered cancel)
// stamps the run cancelled with cancel_reason=handed_off (决策 6-6) — a handoff cut is an
// ownership transition, the reason carries the semantics.
func TestHandoffWindowStampMarksHandedOff(t *testing.T) {
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
	handled := make(chan struct{})
	bus.Subscribe("goal:assigned", func(ctx context.Context, e events.Event) {
		d.onGoalAssigned(ctx, e)
		close(handled)
	})

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentSvc := service.NewAgentService(st, bus)
	agentA, err := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent a: %v", err)
	}
	agentB, err := agentSvc.Create(ctx, service.Agent{Name: "b", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent b: %v", err)
	}
	dom, err := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// A's run is running but NOT registered (the claim→register window).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		"run-a", goal.ID, agentA.ID, "worker", "running", 1, nowStr(), nowStr()); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	if _, err := goalSvc.Assign(ctx, goal.ID, "agent", agentB.ID, "take over", "", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	<-handled

	var status, reason string
	if err := st.DB().QueryRowContext(ctx, `SELECT status, cancel_reason FROM run WHERE id='run-a'`).Scan(&status, &reason); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if status != "cancelled" || reason != "handed_off" {
		t.Fatalf("window-stamped run must be cancelled + cancel_reason=handed_off, got %q / %q", status, reason)
	}
}
