package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// ── Review window (Option B) tests ──
//
// The review RUN enqueue moved INTO the park transaction (service layer,
// 决策 6-19 — see internal/service/squad_review_tx_test.go). What remains
// in the daemon: the reviewer-first approval window — the ready publish
// waits for this window's review runs to go terminal — a stuck reviewer
// dies by the ordinary run lifecycle (idle watchdog / max_run_duration),
// whose terminal event closes the window. The ready publish then patches
// the human's card from the "审查中" hint to the real opinions.

// newSquadReviewDaemon builds the surfaces the review window needs.
func newSquadReviewDaemon(t *testing.T) (*Daemon, *store.Store, *service.GoalService, *service.RunService, *service.SquadService) {
	t.Helper()
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
	squadSvc := service.NewSquadService(st, bus)
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc, squadSvc: squadSvc,
		runCancels: make(map[string]context.CancelFunc), runCancelReasons: make(map[string]string),
		ctx: context.Background()}
	return d, st, goalSvc, runSvc, squadSvc
}

// seedReviewAgent inserts a runtime + agent, returns the agent id.
func seedReviewAgent(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	bus := events.NewBus()
	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt-" + name, MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	a, err := service.NewAgentService(st, bus).Create(ctx, service.Agent{Name: name, RuntimeID: rt.ID, MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return a.ID
}

// squadWithReviewer builds a squad whose member has role=reviewer and a goal
// owned by that squad. Returns goalID + reviewerID.
func squadWithReviewer(t *testing.T, d *Daemon, st *store.Store, gs *service.GoalService, squadSvc *service.SquadService, reviewerRole string) (string, string) {
	t.Helper()
	ctx := context.Background()
	leaderID := seedReviewAgent(t, st, "leader")
	reviewerID := seedReviewAgent(t, st, "reviewer")
	dom, err := service.NewDomainService(st, events.NewBus()).Create(ctx, service.Domain{Name: "review-dom", GitURL: "https://e.com/dom.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	sq, err := squadSvc.Create(ctx, service.Squad{Name: "dev-team", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", reviewerID, reviewerRole); err != nil {
		t.Fatalf("add reviewer member: %v", err)
	}
	g, err := gs.Create(ctx, service.Goal{
		Title: "work", Description: "do the thing",
		DomainID: dom.ID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	return g.ID, reviewerID
}

// TestReviewWindowReady (Option B): goal:review_ready fires only when this
// window's review runs are terminal (or no reviewer exists); the fallback
// timer fires it for a hung reviewer; one ready per window.
func TestReviewWindowReady(t *testing.T) {
	d, st, gs, runSvc, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, reviewerID := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")

	// Park the goal in review and capture ready events.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 8)
	bus := events.NewBus()
	bus.Subscribe("goal:review_ready", func(_ context.Context, e events.Event) {
		m, _ := e.Payload.(map[string]any)
		id, _ := m["goal_id"].(string)
		ready <- id
	})
	d.bus = bus // route the daemon's publishes to the capture

	// A pending review run exists (enqueued in the park tx in production;
	// the direct enqueue stands in for it here) — ready must NOT fire.
	if _, err := runSvc.EnqueueForMentionRole(ctx, goalID, reviewerID, "", "review"); err != nil {
		t.Fatalf("enqueue review run: %v", err)
	}
	var revRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='review' LIMIT 1`,
		goalID, reviewerID).Scan(&revRun); err != nil {
		t.Fatalf("review run: %v", err)
	}
	d.openReviewWindow(ctx, goalID)
	select {
	case <-ready:
		t.Fatal("ready must NOT fire while the review run is pending")
	case <-time.After(50 * time.Millisecond):
	}
	// The review run finishes — ready fires exactly once.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE id=?`, nowStr(), revRun); err != nil {
		t.Fatal(err)
	}
	d.maybeFireReviewReady(ctx, goalID)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("ready must fire once the review run is terminal")
	}
	d.maybeFireReviewReady(ctx, goalID)
	select {
	case <-ready:
		t.Fatal("ready must fire exactly once per window")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestReviewWindowClosesOnReviewerDeath: a "hung" reviewer is NOT special —
// it dies by the ordinary run lifecycle (idle watchdog / max_run_duration /
// human stop), and its terminal event closes the window. No fallback timer:
// the wait is bounded by the reviewer run's own limits, exactly like any
// worker's.
func TestReviewWindowClosesOnReviewerDeath(t *testing.T) {
	d, st, gs, runSvc, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, reviewerID := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}
	if _, err := runSvc.EnqueueForMentionRole(ctx, goalID, reviewerID, "", "review"); err != nil {
		t.Fatalf("enqueue review run: %v", err)
	}
	ready := make(chan string, 8)
	bus := events.NewBus()
	bus.Subscribe("goal:review_ready", func(_ context.Context, e events.Event) {
		m, _ := e.Payload.(map[string]any)
		id, _ := m["goal_id"].(string)
		ready <- id
	})
	d.bus = bus
	d.openReviewWindow(ctx, goalID)
	select {
	case <-ready:
		t.Fatal("ready must NOT fire while the review run is pending")
	case <-time.After(50 * time.Millisecond):
	}
	// The platform kills the stuck reviewer (reaper shape: cancelled run) —
	// the terminal event closes the window, the human's wait ends.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='timeout', finished_at=? WHERE goal_id=? AND role='review'`, nowStr(), goalID); err != nil {
		t.Fatal(err)
	}
	d.onRunTerminal(ctx, events.Event{Payload: map[string]any{"goal_id": goalID}})
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled reviewer run must close the window — the human must not wait forever")
	}
}

// TestStopQueuedRun (D-1 veto): a queued intent — e.g. a mention queued
// during the review freeze — can be stopped by the human before it ever
// claims. The run stamps cancelled+stopped directly (no live process to
// cut); notify skips reason_code=stopped.
func TestStopQueuedRun(t *testing.T) {
	d, st, gs, runSvc, _ := newSquadReviewDaemon(t)
	ctx := context.Background()
	leaderID := seedReviewAgent(t, st, "leader")
	dom, err := service.NewDomainService(st, events.NewBus()).Create(ctx, service.Domain{Name: "d", GitURL: "https://e.com/x.git"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.Create(ctx, service.Goal{Title: "g", AssigneeType: "agent", AssigneeID: leaderID, Status: "active", DomainID: dom.ID})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runSvc.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StopRun(g.ID, run.ID); err != nil {
		t.Fatalf("stop queued run: %v", err)
	}
	var status, reason string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status, cancel_reason FROM run WHERE id=?`, run.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || reason != "stopped" {
		t.Fatalf("queued stop must stamp cancelled+stopped, got %s/%s", status, reason)
	}
	// The goal state is untouched (决策 4-12: the run has no authority).
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("stopping a queued run must not touch the goal, got %q", after.Status)
	}
}

// TestHandoffLoopParkOpensCardWindow: the Assign-side handoff_loop park now
// publishes goal:reviewing (决策 4-4: the human MUST decide the
// collaboration) — the daemon's handler opens the window, no reviewers are
// enqueued (no code change to review), and the ready anchor lands.
func TestHandoffLoopParkOpensCardWindow(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, _ := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")
	// Park as the Assign handoff_loop park does (review_request prefix is the
	// marker), then deliver the event the park publishes.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='handoff_loop: 测试' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}
	d.onGoalReviewing(ctx, events.Event{Payload: map[string]any{
		"goal_id": goalID, "reason": "handoff_loop: 测试",
	}})
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, goalID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a handoff-loop park must not enqueue review runs, got %d", n)
	}
	// The window opened — the review_ready anchor lands for the duration
	// metric (no pending review runs → ready fires immediately).
	var anchor int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM activity_log WHERE goal_id=? AND action='review_ready'`, goalID).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if anchor != 1 {
		t.Fatalf("the ready anchor must be recorded, got %d", anchor)
	}
}
