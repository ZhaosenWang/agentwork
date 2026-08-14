package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// newSquadReviewDaemon builds the surfaces the review trigger needs.
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
	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt-" + name, Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
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

// TestSquadReviewTrigger: a squad-owned goal parking in review triggers a
// system mention to the role=reviewer member + a review run linked to it —
// the platform enforces the squad's own rule (no leader discretion involved).
func TestSquadReviewTrigger(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, reviewerID := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")

	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}

	// The system mention lands in the comment feed with the structured URI.
	var commentID, content string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id, content FROM comment WHERE goal_id=? AND author_type='system'`, goalID).
		Scan(&commentID, &content); err != nil {
		t.Fatalf("system review comment: %v", err)
	}
	if !strings.Contains(content, "[@reviewer](mention://agent/"+reviewerID+")") {
		t.Fatalf("comment should carry the mention URI, got: %q", content)
	}
	if !strings.Contains(content, "do not modify any file") {
		t.Fatalf("comment should bound the reviewer's scope, got: %q", content)
	}

	// The review run is an ordinary mention run, linked to the comment.
	var runID, triggerCommentID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id, trigger_comment_id FROM run WHERE goal_id=? AND agent_id=?`, goalID, reviewerID).
		Scan(&runID, &triggerCommentID); err != nil {
		t.Fatalf("review run: %v", err)
	}
	if triggerCommentID != commentID {
		t.Fatalf("review run must link its trigger comment, got %q want %q", triggerCommentID, commentID)
	}
	_ = runID
}

// TestSquadReviewRoleCaseInsensitive: "Reviewer" (any casing) is recognized —
// the role is a human-written label, not an enum.
func TestSquadReviewRoleCaseInsensitive(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, _ := squadWithReviewer(t, d, st, gs, squadSvc, "Reviewer")

	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, goalID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 review comment, got %d", n)
	}
}

// TestSquadReviewNoReviewerMember: a squad without a role=reviewer member has
// no review rule — nothing is triggered.
func TestSquadReviewNoReviewerMember(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	// Same fixture but the member's role is not reviewer.
	goalID, _ := squadWithReviewer(t, d, st, gs, squadSvc, "writer")

	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, goalID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 0 {
		t.Fatalf("no reviewer rule → no review comment, got %d", n)
	}
}

// TestSquadReviewDedupScopedToReviewRole: the dedupe is scoped to role=
// 'review' (P1-2, 决策 6-15⑧). A pending CONSULT on the reviewer (an agent
// mention) must NOT suppress the platform's review request — a consult is a
// question, the review is the checkpoint opinion; suppressing it leaves the
// round without its reviewer (the Claim gate already serializes them). A
// SECOND park's review request IS a duplicate ask — deduped.
func TestSquadReviewDedupScopedToReviewRole(t *testing.T) {
	d, st, gs, runSvc, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, reviewerID := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")

	// The agent mentions the reviewer first (a consult run — NOT the review).
	if _, err := runSvc.EnqueueForMention(ctx, goalID, reviewerID, "agent-mention-comment"); err != nil {
		t.Fatalf("agent mention: %v", err)
	}
	// The platform's squad-review trigger must STILL fire — the consult does
	// not stand in for the checkpoint opinion.
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	var reviewRuns int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND role='review'`, goalID, reviewerID).Scan(&reviewRuns); err != nil {
		t.Fatalf("count review runs: %v", err)
	}
	if reviewRuns != 1 {
		t.Fatalf("expected the review request to fire despite the pending consult, got %d", reviewRuns)
	}
	// A SECOND park (re-park) must NOT stack another review request.
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review again: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND role='review'`, goalID, reviewerID).Scan(&reviewRuns); err != nil {
		t.Fatalf("count review runs: %v", err)
	}
	if reviewRuns != 1 {
		t.Fatalf("a second park must coalesce into the pending review request, got %d", reviewRuns)
	}
}

// TestSquadReviewSkipsLeaderSelfReview: a reviewer who IS the leader would
// review its own work — the platform excludes it.
func TestSquadReviewSkipsLeaderSelfReview(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()

	leaderID := seedReviewAgent(t, st, "leader")
	dom, err := service.NewDomainService(st, events.NewBus()).Create(ctx, service.Domain{Name: "self-dom", GitURL: "https://e.com/self.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	sq, err := squadSvc.Create(ctx, service.Squad{Name: "self-team", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	// The leader is ALSO declared the reviewer.
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", leaderID, "reviewer"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	g, err := gs.Create(ctx, service.Goal{
		Title: "self work", Description: "do it",
		DomainID: dom.ID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if err := d.maybeTriggerSquadReview(ctx, g.ID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, g.ID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 0 {
		t.Fatalf("leader self-review → no request, got %d", n)
	}
}

// TestSquadReviewNotSquadOwned: an agent-owned goal has no squad rule — no
// review trigger even if the agent happens to be in a squad.
func TestSquadReviewNotSquadOwned(t *testing.T) {
	d, st, gs, _, _ := newSquadReviewDaemon(t)
	ctx := context.Background()
	agentID := seedReviewAgent(t, st, "solo")
	dom, err := service.NewDomainService(st, events.NewBus()).Create(ctx, service.Domain{Name: "solo-dom", GitURL: "https://e.com/solo.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	g, err := gs.Create(ctx, service.Goal{
		Title: "solo work", Description: "do it",
		DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentID, Status: "active",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if err := d.maybeTriggerSquadReview(ctx, g.ID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, g.ID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 0 {
		t.Fatalf("agent-owned goal → no review comment, got %d", n)
	}
}

// TestReviewWindowReady (Option B): goal:review_ready fires only when this
// window's review runs are terminal (or no reviewer exists); the fallback
// timer fires it for a hung reviewer; one ready per window.
func TestReviewWindowReady(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
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

	// A pending review run exists — ready must NOT fire.
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
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

// TestReviewWindowFallbackTimer (Option B): a hung reviewer must not hold
// the human's card hostage — the fallback timer fires ready anyway.
func TestReviewWindowFallbackTimer(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, _ := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	old := reviewReadyFallback
	reviewReadyFallback = 100 * time.Millisecond
	defer func() { reviewReadyFallback = old }()
	ready := make(chan string, 8)
	bus := events.NewBus()
	bus.Subscribe("goal:review_ready", func(_ context.Context, e events.Event) {
		m, _ := e.Payload.(map[string]any)
		id, _ := m["goal_id"].(string)
		ready <- id
	})
	d.bus = bus
	d.openReviewWindow(ctx, goalID) // arms the fallback (a pending review run exists)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("the fallback timer must fire ready for a hung reviewer")
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

// TestHandoffLoopParkFiresCardEvent: the ≥8 handoff park publishes
// goal:reviewing (the human must decide) — while the squad checkpoint skips
// it (no code change to review).
func TestHandoffLoopParkFiresCardEvent(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, _ := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")
	// Park as the handoff-loop park does (review_request prefix is the marker).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='handoff_loop: 测试' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}
	// The squad checkpoint skips handoff_loop parks — no review run.
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, goalID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a handoff-loop park must not trigger squad review runs, got %d", n)
	}
	// The window still opens (the card fires with no hint).
	d.openReviewWindow(ctx, goalID)
	// And the review_ready anchor lands for the duration metric.
	var anchor int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM activity_log WHERE goal_id=? AND action='review_ready'`, goalID).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if anchor != 1 {
		t.Fatalf("the ready anchor must be recorded, got %d", anchor)
	}
}
