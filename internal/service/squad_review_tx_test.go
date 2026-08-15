package service

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// enqueueReview opens a transaction and runs enqueueSquadReviewTx — the
// short path for tests that don't drive the park transaction themselves.
func enqueueReview(t *testing.T, gs *GoalService, st *store.Store, goalID, parkRunID, reason string) {
	t.Helper()
	tx, err := st.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := gs.enqueueSquadReviewTx(context.Background(), tx, goalID, parkRunID, reason); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// ── Squad review enqueue IN the park transaction (决策 6-13/6-19) ──
//
// The review run is the park's SUCCESSOR: it is born in the park's
// transaction, so when goal:reviewing publishes, the run already exists —
// the approval card's pending-reviewer hint never races an empty list (the
// live failure: an event-handler enqueue raced the notify handler and the
// card fired without the "审查中" hint while the reviewer was starting).

func TestEnqueueSquadReviewTxAnchorsOnCompletionDeclaration(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	leaderID := seedAgent(t, st, "leader")
	reviewerID := seedAgent(t, st, "reviewer")
	domID := seedDomain(t, st)
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "dev-team", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", reviewerID, "reviewer"); err != nil {
		t.Fatalf("add reviewer: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// The parking owner run completes with its report — the completion
	// declaration that anchors the review.
	var ownerRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' LIMIT 1`, g.ID).Scan(&ownerRun); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE id=?`, now(), ownerRun); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?, 'agent',?,NULL,?,?,?)`,
		"report-1", g.ID, leaderID, "任务完成，等待判定。", now(), ownerRun); err != nil {
		t.Fatal(err)
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := gs.enqueueSquadReviewTx(ctx, tx, g.ID, ownerRun, "merge: test"); err != nil {
		t.Fatalf("enqueue squad review: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// The review run exists BEFORE any event publishes — role='review' (the
	// explicit stamp), anchored on the completion declaration.
	var role, trigger string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT role, trigger_comment_id FROM run WHERE goal_id=? AND agent_id=?`, g.ID, reviewerID).
		Scan(&role, &trigger); err != nil {
		t.Fatalf("review run: %v", err)
	}
	if role != "review" {
		t.Fatalf("the review run keeps its review role, got %q", role)
	}
	if trigger != "report-1" {
		t.Fatalf("the review run anchors on the completion declaration, got %q", trigger)
	}
	// No platform comment minted (决策 6-19).
	var sysComments int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, g.ID).Scan(&sysComments); err != nil {
		t.Fatal(err)
	}
	if sysComments != 0 {
		t.Fatalf("no platform comment for the review request, got %d", sysComments)
	}
	_ = rs
}

func TestEnqueueSquadReviewTxSkipsHandoffLoopAndNonSquad(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leaderID := seedAgent(t, st, "leader")
	reviewerID := seedAgent(t, st, "reviewer")
	domID := seedDomain(t, st)
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "dev-team", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", reviewerID, "reviewer"); err != nil {
		t.Fatalf("add reviewer: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// A handoff_loop park has no code change to review — no runs.
	enqueueReview(t, gs, st, g.ID, "", "handoff_loop: 所有权已交接 8 次")
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, g.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a handoff-loop park must not enqueue reviewers, got %d", n)
	}

	// An agent-owned goal has no squad rule — no runs.
	agentID := seedAgent(t, st, "solo")
	g2, err := gs.Create(ctx, Goal{Title: "solo", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentID, Status: "active"})
	if err != nil {
		t.Fatalf("create agent goal: %v", err)
	}
	enqueueReview(t, gs, st, g2.ID, "", "merge: test")
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, g2.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("an agent-owned goal must not enqueue reviewers, got %d", n)
	}
}

func TestEnqueueSquadReviewTxExcludesLeaderAndDedupes(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leaderID := seedAgent(t, st, "leader")
	domID := seedDomain(t, st)
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "self-team", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	// The leader IS also the declared reviewer — self-review excluded.
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", leaderID, "reviewer"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "self", Description: "do it", DomainID: domID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	enqueueReview(t, gs, st, g.ID, "", "merge: test")
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, g.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a leader must not review its own work, got %d", n)
	}

	// A pending review request dedupes the second enqueue.
	otherID := seedAgent(t, st, "other")
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", otherID, "reviewer"); err != nil {
		t.Fatalf("add second reviewer: %v", err)
	}
	enqueueReview(t, gs, st, g.ID, "", "merge: test")
	enqueueReview(t, gs, st, g.ID, "", "merge: test")
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, g.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a second enqueue must coalesce into the pending review request, got %d", n)
	}
}

// TestHandoffLoopParkPublishesReviewing: the Assign-side handoff_loop park
// publishes goal:reviewing (决策 4-4: the human MUST decide) — the daemon
// opens the card window from it; reviewers are NOT enqueued (handoff_loop
// prefix). The live failure: handoffLoopEvs was declared but never appended,
// so the card fired only after a daemon restart.
func TestHandoffLoopParkPublishesReviewing(t *testing.T) {
	st := newTestStore(t)
	bus := events.NewBus()
	gs := NewGoalService(st, bus)
	rs := NewRunService(st, bus)
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	b := seedAgent(t, st, "B")
	g, err := gs.Create(ctx, Goal{Title: "churn", Description: "do it", DomainID: seedDomain(t, st), AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	// 8 ownership transitions: the park threshold.
	for i := 0; i < 8; i++ {
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO handoff_event (id,goal_id,from_type,from_id,to_type,to_id,from_run_id,to_run_id,reason,actor_type,actor_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			newID(), g.ID, "agent", a, "agent", b, "", "", "r", "human", "", now()); err != nil {
			t.Fatal(err)
		}
	}
	reviewing := make(chan string, 4)
	bus.Subscribe("goal:reviewing", func(_ context.Context, e events.Event) {
		m, _ := e.Payload.(map[string]any)
		id, _ := m["goal_id"].(string)
		reviewing <- id
	})
	if _, err := gs.Assign(ctx, g.ID, "agent", b, "take it", "human", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-reviewing:
		if id != g.ID {
			t.Fatalf("reviewing event for the wrong goal: %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handoff_loop park must publish goal:reviewing — the human's card depends on it")
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "review" {
		t.Fatalf("the park must land the goal in review, got %q", after.Status)
	}
	// No review runs (no code change to review).
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review'`, g.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a handoff-loop park must not enqueue reviewers, got %d", n)
	}
}

// TestReviewPhaseDerivation (决策 6-19 延伸): the Goal API derives the
// review window's phase from the goal's own review runs — the Web renders
// 待审查/审查中/待审批 from it, and the approval buttons appear only at
// awaiting_approval.
func TestReviewPhaseDerivation(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentID := seedAgent(t, st, "worker")
	reviewer := seedAgent(t, st, "reviewer")
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: seedDomain(t, st), AssigneeType: "agent", AssigneeID: agentID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	getPhase := func() string {
		t.Helper()
		gg, err := gs.Get(ctx, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		return gg.ReviewPhase
	}
	if p := getPhase(); p != "" {
		t.Fatalf("a non-review goal has no phase, got %q", p)
	}

	// Park + enqueue a review run: queued → awaiting_review.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if p := getPhase(); p != "awaiting_approval" {
		t.Fatalf("no reviewer → the human approves directly, got %q", p)
	}
	if _, err := rs.EnqueueForMentionRole(ctx, g.ID, reviewer, "", "review"); err != nil {
		t.Fatal(err)
	}
	if p := getPhase(); p != "awaiting_review" {
		t.Fatalf("a queued review run → awaiting_review, got %q", p)
	}
	// Claimed → reviewing.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE goal_id=? AND role='review'`, now(), g.ID); err != nil {
		t.Fatal(err)
	}
	if p := getPhase(); p != "reviewing" {
		t.Fatalf("a running review run → reviewing, got %q", p)
	}
	// Terminal → awaiting_approval (opinions are in).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE goal_id=? AND role='review'`, now(), g.ID); err != nil {
		t.Fatal(err)
	}
	if p := getPhase(); p != "awaiting_approval" {
		t.Fatalf("terminal review runs → awaiting_approval, got %q", p)
	}
}

// TestDispatchOnlyRunLeavesNoReport (决策 4-6/6-22 修订): an owner run whose
// only action was dispatching sub-goals (no trigger comment — neither a
// mention nor a reply) leaves NO flat report in the feed. The dispatch
// comments already announced the delegation; the flat "已派发…" report was
// the feed noise the live runs showed.
func TestDispatchOnlyRunLeavesNoReport(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	worker := seedAgent(t, st, "worker")
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "do it", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	// The owner run is LIVE first (claimed)…
	var ownerRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' LIMIT 1`, g.ID).Scan(&ownerRun); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE id=?`, now(), ownerRun); err != nil {
		t.Fatal(err)
	}
	// …then it dispatches during its run window…
	if _, err := gs.CreateSubGoal(ctx, g.ID, "审计用例", "补齐测试", worker, "", "agent", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE id=?`, now(), ownerRun); err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	report, err := insertRunResultComment(ctx, tx, goalRunContext{
		RunID: ownerRun, GoalID: g.ID, AgentID: owner, Role: "owner", Status: "completed",
		Summary: "已派发子任务给 coder。",
	})
	if err == nil {
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if report != "" {
		t.Fatalf("a dispatch-only owner run must leave no flat report, got %q", report)
	}
	// The dispatch comment itself is still there (the platform's announce);
	// the owner's bare report text must NOT appear anywhere.
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND content LIKE '%已派发子任务给 coder%'`, g.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the dispatch-only owner report must not land in the feed, got %d", n)
	}
}
