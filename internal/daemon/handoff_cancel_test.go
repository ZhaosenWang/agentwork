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
	if _, err := goalSvc.Assign(ctx, goal.ID, "agent", agentB.ID, "take over"); err != nil {
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

	if _, err := goalSvc.Assign(ctx, goal.ID, "human", "", ""); err != nil {
		t.Fatalf("assign to human: %v", err)
	}
	<-handled
	if !fired {
		t.Fatalf("running run must be cut when the goal returns to human")
	}
}

// TestApprovalCutTerminatesRunningRun: a goal entering review (agent `goal
// request-approval` publishes goal:reviewing) must not carry a live agent
// run — it would keep mutating the worktree the approval judges, and it
// would block the review run behind per-goal serialization. The platform
// cuts it (reason "approval": normal control flow, not a stall).
func TestApprovalCutTerminatesRunningRun(t *testing.T) {
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
	bus.Subscribe("goal:reviewing", func(ctx context.Context, e events.Event) {
		d.onGoalReviewing(ctx, e)
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

	// RequestApproval parks the goal in review and publishes goal:reviewing —
	// the daemon cuts the agent's own still-running run.
	if _, err := goalSvc.RequestApproval(ctx, goal.ID, "please check"); err != nil {
		t.Fatalf("request approval: %v", err)
	}
	<-handled
	if !fired {
		t.Fatalf("running run must be cut when the goal enters review")
	}
}
