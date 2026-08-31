package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// seedWatchdogCtx builds a daemon with bus + commentSvc + runSvc wired so
// machineWatchdogTick can post its idle-notify comment and read run status.
func seedWatchdogCtx(t *testing.T) (*Daemon, *store.Store, string, string, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	d := &Daemon{
		st:                 st,
		bus:                bus,
		machineLastEvent:   make(map[string]time.Time),
		machineCancels:     make(map[string][]link.RunCancelParams),
		machinePollWake:    make(map[string]chan struct{}),
		machineLastEventMu: sync.Mutex{},
		machinePendingMu:   sync.Mutex{},
	}
	d.commentSvc = service.NewCommentService(st, bus)
	d.runSvc = service.NewRunService(st, bus)
	d.commentSvc.SetGoalService(service.NewGoalService(st, bus))

	rt, err := service.NewRuntimeService(st).Create(context.Background(), service.Runtime{Name: "rt", MachineID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := service.NewAgentService(st, bus).Create(context.Background(), service.Agent{Name: "A", RuntimeID: rt.ID})
	if err != nil {
		t.Fatal(err)
	}
	dom, err := service.NewDomainService(st, bus).Create(context.Background(), service.Domain{Name: "d", GitURL: "https://example.com/d.git", PolicyText: "测试能过"})
	if err != nil {
		t.Fatal(err)
	}
	gs := service.NewGoalService(st, bus)
	gs.SetRunService(d.runSvc)
	g, err := gs.Create(context.Background(), service.Goal{Title: "g", Description: "desc", AssigneeType: "agent", AssigneeID: agent.ID, Status: "active", DomainID: dom.ID})
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-watchdog-test"
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,queued_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		runID, g.ID, agent.ID, "worker", "worker", "running", "owner", 1, "", "", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert test run: %v", err)
	}
	return d, st, g.ID, agent.ID, runID
}

// TestWatchdogIdlePostsCommentAndDoesNotCancel: a run silent beyond idleWindow
// must NOT be cancelled — instead a single system comment is posted to the goal
// feed. This is the core fix: the platform no longer unilaterally cancels on
// idle.
func TestWatchdogIdlePostsCommentAndDoesNotCancel(t *testing.T) {
	d, st, goalID, _, runID := seedWatchdogCtx(t)

	// Simulate the run having gone quiet 3 minutes ago (> idleWindow=2min).
	d.machineLastEventMu.Lock()
	d.machineLastEvent[runID] = time.Now().Add(-3 * time.Minute)
	d.machineLastEventMu.Unlock()

	start := time.Now()
	cancelSent := time.Time{}
	idleNotified := false

	// First tick: should fire idle-notify.
	stop := d.machineWatchdogTick(runID, "m1", start, &cancelSent, &idleNotified)
	if stop {
		t.Error("idle tick must not stop the watchdog")
	}
	if !idleNotified {
		t.Error("idleNotified should be true after idle tick")
	}

	// The run must still be running — idle does not cancel.
	var status string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM run WHERE id=?`, runID).Scan(&status); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if status != "running" {
		t.Errorf("idle watchdog must not cancel: run status = %q, want %q", status, "running")
	}

	// Exactly one system comment must have been posted.
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system' AND run_id=?`,
		goalID, runID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 idle-notify system comment, got %d", n)
	}

	// No cancel must have been enqueued.
	d.machinePendingMu.Lock()
	cancels := len(d.machineCancels["m1"])
	d.machinePendingMu.Unlock()
	if cancels != 0 {
		t.Errorf("idle watchdog must not enqueue cancel: got %d", cancels)
	}

	// Second tick: must NOT post a second comment (idleNotified guards it).
	d.machineWatchdogTick(runID, "m1", start, &cancelSent, &idleNotified)
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system' AND run_id=?`,
		goalID, runID).Scan(&n); err != nil {
		t.Fatalf("count comments on 2nd tick: %v", err)
	}
	if n != 1 {
		t.Errorf("idle-notify must fire once: got %d comments after 2nd tick", n)
	}
}

// TestWatchdogMaxRunDurationCancels: a run exceeding 2h total duration must
// still be cancelled — the declared-budget contract is the platform's only
// unilateral cancellation authority.
func TestWatchdogMaxRunDurationCancels(t *testing.T) {
	d, st, _, _, runID := seedWatchdogCtx(t)

	// Keep lastEvent fresh so the idle path doesn't fire first.
	d.machineLastEventMu.Lock()
	d.machineLastEvent[runID] = time.Now()
	d.machineLastEventMu.Unlock()

	// Simulate the run having started 3h ago (budget exceeded).
	start := time.Now().Add(-3 * time.Hour)
	cancelSent := time.Time{}
	idleNotified := false

	stop := d.machineWatchdogTick(runID, "m1", start, &cancelSent, &idleNotified)
	if stop {
		t.Error("first max_run_duration tick should not stop (sends cancel, waits grace)")
	}

	// A cancel must have been enqueued.
	d.machinePendingMu.Lock()
	cancels := len(d.machineCancels["m1"])
	d.machinePendingMu.Unlock()
	if cancels != 1 {
		t.Fatalf("max_run_duration must enqueue 1 cancel: got %d", cancels)
	}

	// cancel_reason must be stamped 'timeout'.
	var reason string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT cancel_reason FROM run WHERE id=?`, runID).Scan(&reason); err != nil {
		t.Fatalf("query cancel_reason: %v", err)
	}
	if reason != "timeout" {
		t.Errorf("cancel_reason = %q, want %q", reason, "timeout")
	}

	// The run must still be running (cancel sent, grace not yet expired).
	var status string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM run WHERE id=?`, runID).Scan(&status); err != nil {
		t.Fatalf("query run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run should still be running during grace: got %q", status)
	}
}

// TestWatchdogTerminalRunStops: if the run is no longer 'running', the tick
// must return true (stop the watchdog) without posting or cancelling anything.
func TestWatchdogTerminalRunStops(t *testing.T) {
	d, st, goalID, _, runID := seedWatchdogCtx(t)

	// Flip the run to completed before ticking.
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE run SET status='completed' WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	cancelSent := time.Time{}
	idleNotified := false

	stop := d.machineWatchdogTick(runID, "m1", start, &cancelSent, &idleNotified)
	if !stop {
		t.Error("terminal run must stop the watchdog (return true)")
	}
	if idleNotified {
		t.Error("terminal tick must not set idleNotified")
	}

	// No comment, no cancel.
	var n int
	st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, goalID).Scan(&n)
	if n != 0 {
		t.Errorf("terminal tick must not post comment: got %d", n)
	}
	d.machinePendingMu.Lock()
	cancels := len(d.machineCancels["m1"])
	d.machinePendingMu.Unlock()
	if cancels != 0 {
		t.Errorf("terminal tick must not enqueue cancel: got %d", cancels)
	}
}
