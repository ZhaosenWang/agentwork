package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/link"
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

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
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

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
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

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
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
// stamps the run cancelled with cancel_reason=handoff (决策 6-6) — a handoff cut is an
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

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
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
	if status != "cancelled" || reason != "handoff" {
		t.Fatalf("window-stamped run must be cancelled + cancel_reason=handoff, got %q / %q", status, reason)
	}
}

// TestIngestRunFinishedRecordsSessionOnHandoffCutRun is the regression for the
// "agent 像没有记忆一样" handoff bug. A handoff cut stamps the old owner's run
// 'cancelled' in onGoalAssigned BEFORE the executor's terminal report arrives
// (the executor was still mid-turn when the goal changed hands). IngestRunFinished
// must still record the session id + workdir — they are the resume pointer the
// NEXT writable run of the same (goal, agent) carries. The old code ran
// MarkSession AFTER the `status != "running"` guard, so a handoff-cut run's
// session was dropped and the next run started with no memory.
func TestIngestRunFinishedRecordsSessionOnHandoffCutRun(t *testing.T) {
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

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentA, err := service.NewAgentService(st, bus).Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent a: %v", err)
	}
	agentB, err := service.NewAgentService(st, bus).Create(ctx, service.Agent{Name: "b", RuntimeID: rt.ID})
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
	// A's run is running and HAS a registered cancel (the normal handoff path,
	// not the window stamp) — onGoalAssigned cancels it in-memory.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,token,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-a", goal.ID, agentA.ID, "worker", "running", 1, "tok-a", nowStr(), nowStr()); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	var cut bool
	d.mu.Lock()
	d.runCancels["run-a"] = func() { cut = true }
	d.mu.Unlock()

	// Hand off A → B: the daemon cancels A's run in-memory (no DB stamp here
	// — the cancel flows through the executor's terminal report below).
	if _, err := goalSvc.Assign(ctx, goal.ID, "agent", agentB.ID, "take over", "", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	<-handled
	if !cut {
		t.Fatalf("old owner's running run was not cancelled")
	}

	// The executor, still mid-turn, now reports its terminal state. It carries
	// the session id + workdir it was using. The run row is still 'running'
	// (the in-memory cancel did not stamp the DB — the executor's report is
	// the terminal stamp). MarkSession MUST fire here.
	if rerr := d.IngestRunFinished(ctx, "m1", link.RunFinishedParams{
		RunID: "run-a", Status: "cancelled", Token: "tok-a",
		SessionID: "sess-a-123", WorkDir: "/home/.agentwork/runs/goal/g1/a",
	}); rerr != nil {
		t.Fatalf("IngestRunFinished: %v", rerr)
	}
	var sid, wd string
	if err := st.DB().QueryRowContext(ctx, `SELECT session_id, workdir FROM run WHERE id='run-a'`).Scan(&sid, &wd); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if sid != "sess-a-123" || wd != "/home/.agentwork/runs/goal/g1/a" {
		t.Fatalf("handoff-cut run's session pointer was not recorded: got session=%q workdir=%q — the next run of this (goal, agent) starts with no memory", sid, wd)
	}

	// The hard version: the claim→register window stamp path pre-stamps the
	// run 'cancelled' in the DB (onGoalAssigned's fallback when no cancel is
	// registered). IngestRunFinished must STILL record the session pointer
	// despite status != "running" — the old code's guard returned before
	// MarkSession and dropped it.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,token,cancel_reason,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"run-c", goal.ID, agentA.ID, "worker", "cancelled", 1, "tok-c", "handoff", nowStr(), nowStr()); err != nil {
		t.Fatalf("insert pre-stamped run: %v", err)
	}
	if rerr := d.IngestRunFinished(ctx, "m1", link.RunFinishedParams{
		RunID: "run-c", Status: "cancelled", Token: "tok-c",
		SessionID: "sess-c-456", WorkDir: "/home/.agentwork/runs/goal/g1/c",
	}); rerr != nil {
		t.Fatalf("IngestRunFinished (pre-stamped): %v", rerr)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT session_id, workdir FROM run WHERE id='run-c'`).Scan(&sid, &wd); err != nil {
		t.Fatalf("load pre-stamped run: %v", err)
	}
	if sid != "sess-c-456" || wd != "/home/.agentwork/runs/goal/g1/c" {
		t.Fatalf("pre-stamped cancelled run's session pointer was not recorded: got session=%q workdir=%q — the window-stamp path lost the memory too", sid, wd)
	}
}

// TestIngestRunFinishedStaleTokenDoesNotOverwriteSession: a stale terminal
// report (the run was re-claimed with a fresh token after the executor died)
// must NOT overwrite the session pointer. The old code ran MarkSession before
// the token check; a stale report from the dead executor would stamp its
// (possibly different) session id over the run row, and the next priorSessionFor
// read could pick up the wrong session. MarkSession must run AFTER the token
// gate so only the current run's report writes the pointer.
func TestIngestRunFinishedStaleTokenDoesNotOverwriteSession(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	runSvc := service.NewRunService(st, bus)
	d := &Daemon{st: st, bus: bus, runSvc: runSvc, ctx: context.Background()}
	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
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
	goalSvc := service.NewGoalService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// A running run with the CURRENT token. An earlier executor (token
	// "tok-stale") was re-claimed; its late report must not write session.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,attempt,token,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"run-a", goal.ID, agentA.ID, "worker", "running", 1, "tok-current", nowStr(), nowStr()); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// The stale report arrives with the OLD token + a session id.
	if rerr := d.IngestRunFinished(ctx, "m1", link.RunFinishedParams{
		RunID: "run-a", Status: "completed", Token: "tok-stale",
		SessionID: "sess-stale", WorkDir: "/stale",
	}); rerr != nil {
		t.Fatalf("IngestRunFinished stale: %v", rerr)
	}
	var sid, wd string
	if err := st.DB().QueryRowContext(ctx, `SELECT session_id, workdir FROM run WHERE id='run-a'`).Scan(&sid, &wd); err != nil {
		t.Fatalf("load run: %v", err)
	}
	if sid == "sess-stale" || wd == "/stale" {
		t.Fatalf("stale-token report must not overwrite the session pointer: got session=%q workdir=%q — a re-claimed run's dead executor stamped its session over the current run", sid, wd)
	}
	// The current-token report DOES write the session (the happy path still works).
	if rerr := d.IngestRunFinished(ctx, "m1", link.RunFinishedParams{
		RunID: "run-a", Status: "completed", Token: "tok-current",
		SessionID: "sess-current", WorkDir: "/current",
	}); rerr != nil {
		t.Fatalf("IngestRunFinished current: %v", rerr)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT session_id, workdir FROM run WHERE id='run-a'`).Scan(&sid, &wd); err != nil {
		t.Fatalf("load run after current: %v", err)
	}
	if sid != "sess-current" || wd != "/current" {
		t.Fatalf("current-token report must record the session pointer: got session=%q workdir=%q", sid, wd)
	}
}

// TestPriorSessionSkipsCancelledRunSession: the resume pointer must come
// from a COMPLETED run only. A cancelled run (handoff cut / watchdog) may
// carry a session POISONED by a killed turn — an empty assistant message in
// the CLI's persisted history fails every future prompt (the executor's
// fresh-fallback exists for this). Resuming a known-poisoned session is a
// wasted failure + memory loss. When a newer cancelled run sits on top of an
// older completed run, priorSessionFor must pick the completed one; when only
// cancelled runs exist, it returns "" (fresh start, preferable to poison).
func TestPriorSessionSkipsCancelledRunSession(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	d := &Daemon{st: st, bus: bus, ctx: context.Background()}
	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", MachineID: "m1"})
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
	goalSvc := service.NewGoalService(st, bus)
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// An older COMPLETED run with a clean session.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,session_id,workdir,finished_at,queued_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"run-done", goal.ID, agentA.ID, "worker", "completed", "owner", 1,
		"sess-clean", "/wd-clean", "2026-08-17T10:00:00Z", "2026-08-17T09:00:00Z", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert completed run: %v", err)
	}
	// A NEWER cancelled run (handoff cut) whose session may be poisoned.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,session_id,workdir,finished_at,queued_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"run-cut", goal.ID, agentA.ID, "worker", "cancelled", "owner", 2,
		"sess-maybe-poisoned", "/wd-cut", "2026-08-17T11:00:00Z", "2026-08-17T10:30:00Z", "2026-08-17T10:30:00Z"); err != nil {
		t.Fatalf("insert cancelled run: %v", err)
	}
	sid, wd := d.priorSessionFor(ctx, goal.ID, agentA.ID, "")
	if sid != "sess-clean" || wd != "/wd-clean" {
		t.Fatalf("priorSessionFor must pick the COMPLETED run's clean session, not the newer cancelled run's possibly-poisoned one: got session=%q workdir=%q", sid, wd)
	}

	// When ONLY cancelled runs exist, the pointer stays empty — a fresh start
	// beats resuming a session that may be poisoned.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM run WHERE id='run-done'`); err != nil {
		t.Fatalf("delete completed run: %v", err)
	}
	sid, _ = d.priorSessionFor(ctx, goal.ID, agentA.ID, "")
	if sid != "" {
		t.Fatalf("priorSessionFor must return empty when no completed run exists (never resume a cancelled run's session): got session=%q", sid)
	}
}
