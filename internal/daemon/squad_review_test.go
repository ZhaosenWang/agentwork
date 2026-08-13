package daemon

import (
	"context"
	"strings"
	"testing"

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
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc, squadSvc: squadSvc}
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

// TestSquadReviewDedupWithExistingMention: when the agent already mentioned
// the reviewer (a pending run exists), the platform's squad-review trigger
// must NOT post a duplicate request — the ask is already out.
func TestSquadReviewDedupWithExistingMention(t *testing.T) {
	d, st, gs, runSvc, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, reviewerID := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")

	// The agent mentions the reviewer first (its own comment + run).
	if _, err := runSvc.EnqueueForMention(ctx, goalID, reviewerID, "agent-mention-comment"); err != nil {
		t.Fatalf("agent mention: %v", err)
	}
	// The platform's squad-review trigger must now be a no-op.
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}

	var runs, sysComments int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=?`, goalID, reviewerID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("expected exactly 1 review run (no duplicate), got %d", runs)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, goalID).Scan(&sysComments); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if sysComments != 0 {
		t.Fatalf("no duplicate system review comment expected, got %d", sysComments)
	}
}

// TestSquadReviewSkipsWorktreePark: worktree-dirty park is a platform problem
// (the run never started) — there is no finished work to review, so no
// review request may fire.
func TestSquadReviewSkipsWorktreePark(t *testing.T) {
	d, st, gs, _, squadSvc := newSquadReviewDaemon(t)
	ctx := context.Background()
	goalID, _ := squadWithReviewer(t, d, st, gs, squadSvc, "reviewer")

	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET review_request=? WHERE id=?`,
		"worktree 有未归因的改动（可能是手动编辑）：\n M secret.txt\n请检查 worktree 后批准继续或驳回。", goalID); err != nil {
		t.Fatalf("park goal: %v", err)
	}
	if err := d.maybeTriggerSquadReview(ctx, goalID); err != nil {
		t.Fatalf("trigger squad review: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system'`, goalID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if n != 0 {
		t.Fatalf("worktree-dirty park → no review request, got %d", n)
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
