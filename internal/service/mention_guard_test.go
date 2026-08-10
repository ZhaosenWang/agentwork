package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
)

// TestMentionCycleFailsGoal: agent-to-agent mention churn above the hard
// threshold (MaxMentionCycle) fails the goal at the NEXT trigger — the run is
// refused, the failure reason names the cycle count, queued runs are dropped,
// and the failure lands in the feed. A↔B alternation with cancelled finishes
// (cancelled runs do not advance the goal) keeps the goal active throughout.
func TestMentionCycleFailsGoal(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "churn", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	mention := func(from, to string, i int) {
		t.Helper()
		if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: from, Content: fmt.Sprintf("[@X](mention://agent/%s) round %d", to, i)}); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
	}
	// A ↔ B alternation, each run finished cancelled (no goal advance).
	for i := 0; i < MaxMentionCycle; i++ {
		from, to := agentA, agentB
		if i%2 == 1 {
			from, to = agentB, agentA
		}
		mention(from, to, i)
		runs, _ := rs.List(ctx, g.ID)
		last := runs[len(runs)-1]
		if err := rs.Finish(ctx, last.ID, "cancelled", "stopped"); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	// The next trigger (cycle+1) is refused and fails the goal.
	mention(agentA, agentB, MaxMentionCycle)
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "failed" {
		t.Fatalf("churn must fail the goal, got %q", after.Status)
	}
	// The refused run was not created: runs on B == half the cycle.
	runs, _ := rs.List(ctx, g.ID)
	bRuns := 0
	for _, r := range runs {
		if r.AgentID == agentB {
			bRuns++
		}
	}
	if bRuns != MaxMentionCycle/2 {
		t.Fatalf("expected %d runs on B (last refused), got %d", MaxMentionCycle/2, bRuns)
	}
	// The failure reason + feed comment exist.
	var sysComment string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT content FROM comment WHERE goal_id=? AND author_type='system' ORDER BY created_at DESC LIMIT 1`, g.ID).Scan(&sysComment); err != nil {
		t.Fatalf("system failure comment: %v", err)
	}
	if !strings.Contains(sysComment, fmt.Sprintf("协作循环 %d 次", MaxMentionCycle)) {
		t.Fatalf("failure comment should name the cycle count, got: %q", sysComment)
	}
}

// TestGuestFailedRunLeavesTrace: a mention-triggered (guest) run that FAILS
// leaves a system trace in the feed — the human waiting at a checkpoint sees
// "the collaboration run failed" instead of an empty request.
func TestGuestFailedRunLeavesTrace(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: "[@B](mention://agent/" + agentB + ") 帮个忙"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 1 || runs[0].AgentID != agentB {
		t.Fatalf("expected 1 guest run on B, got %d", len(runs))
	}
	if err := rs.Finish(ctx, runs[0].ID, "failed", "boom: broken model"); err != nil {
		t.Fatalf("finish guest failed: %v", err)
	}
	var content string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT content FROM comment WHERE goal_id=? AND author_type='system'`, g.ID).Scan(&content); err != nil {
		t.Fatalf("system trace comment: %v", err)
	}
	if !strings.Contains(content, "协作 run 失败") || !strings.Contains(content, "broken model") {
		t.Fatalf("trace should carry the failure, got: %q", content)
	}
	// The guest failure must NOT advance the goal.
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("guest failure must not advance the goal, got %q", after.Status)
	}
}

// TestSquadLeaderMentionedRunHasAuthority: a squad leader mentioned BY NAME
// (mention://agent/<leader>, not a leader-run mark) still owns the goal — its
// completed run advances the goal (parks in review on the merge gate). The
// authority follows the assignee relationship, judged dynamically.
func TestSquadLeaderMentionedRunHasAuthority(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	leaderID := seedAgent(t, st, "L")
	domID := seedDomainWithGates(t, st)
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "squad work", Description: "do it", DomainID: domID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: "someone", Content: "[@L](mention://agent/" + leaderID + ") 你来处理"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.IsLeaderRun {
		t.Fatal("mention run must NOT carry the leader mark (authority is dynamic)")
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET gates_hit=? WHERE id=?`, `["merge: 合并到主分支前必须人工审批"]`, r.ID); err != nil {
		t.Fatalf("set gates: %v", err)
	}
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "review" {
		t.Fatalf("leader's mentioned run must advance the goal to review, got %q", after.Status)
	}
}

// TestDoneCancelsQueuedRuns: a goal reaching done drops its queued runs in
// the same transaction — a mention run that raced ahead must not be claimed
// onto a finished goal.
func TestDoneCancelsQueuedRuns(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// B's mention run queues ahead of A's completion.
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: "[@B](mention://agent/" + agentB + ") 排队"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("goal should be done, got %q", after.Status)
	}
	runs, _ := rs.List(ctx, g.ID)
	for _, run := range runs {
		if run.AgentID == agentB && run.Status != "cancelled" {
			t.Fatalf("queued mention run must be cancelled on done, got %q", run.Status)
		}
	}
}

// TestWaitChildrenCancelsQueuedRuns: waiting on sub-goals drops queued runs —
// a blocked goal is not working.
func TestWaitChildrenCancelsQueuedRuns(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: "[@B](mention://agent/" + agentB + ") 排队"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if err := gs.WaitChildren(ctx, g.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	for _, run := range runs {
		if run.AgentID == agentB && run.Status != "cancelled" {
			t.Fatalf("queued mention run must be cancelled on wait, got %q", run.Status)
		}
	}
}
