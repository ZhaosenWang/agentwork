package service

import (
	"context"
	"strings"
	"testing"
)

// TestCreateWritesAssignmentComment: creating an active goal with a
// description lands the assignment instruction in the comment feed — the
// creation event is a coordination event, same as a mention, and the feed is
// where coordination is recorded (visible to humans and injected into the
// agent's run prompt).
func TestCreateWritesAssignmentComment(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "writer")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{
		Title:        "add string utils",
		Description:  "作为 dev-team leader：给 test-repo 添加 string_utils.py + 对应测试",
		DomainID:     domID,
		AssigneeType: "agent",
		AssigneeID:   agentA,
		Status:       "active",
		CreatedByType: "human",
		CreatedByID:  "ui",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}

	comments, err := NewCommentService(st, nil).List(ctx, g.ID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 creation comment, got %d", len(comments))
	}
	c := comments[0]
	if c.AuthorType != "human" || c.AuthorID != "ui" {
		t.Fatalf("comment author = %s/%s, want human/ui", c.AuthorType, c.AuthorID)
	}
	// The creation comment is a MENTION — same shape an agent produces:
	// [@Name](mention://agent/<id>) + instruction. Uniform coordination.
	if want := "[@writer](mention://agent/" + agentA + ")"; !strings.Contains(c.Content, want) {
		t.Fatalf("comment should be a structured mention %q, got: %q", want, c.Content)
	}
	if !strings.Contains(c.Content, "string_utils.py") {
		t.Fatalf("comment should carry the description, got: %q", c.Content)
	}
	// The assignee's run is enqueued by the caller (handler), never by Create
	// itself — and the creation comment must NOT have dispatched anything.
	_ = rs
}

// TestCreateCommentDoesNotDispatchMentions: a description that mentions
// another agent must not double-trigger at creation — the assignee's run is
// the only run; the mention URI in the creation comment is inert (it reads
// as a directive for the assignee, not a live trigger).
func TestCreateCommentDoesNotDispatchMentions(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "writer")
	agentB := seedAgent(t, st, "reviewer")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{
		Title:        "work with review",
		Description:  "做完后把审查交给 [@reviewer](mention://agent/" + agentB + ")",
		DomainID:     domID,
		AssigneeType: "agent",
		AssigneeID:   agentA,
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=?`, g.ID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("creation must not enqueue runs (the caller does), got %d", n)
	}
	// The mention lives in the comment as written, inert.
	cs := NewCommentService(st, nil)
	comments, err := cs.List(ctx, g.ID)
	if err != nil || len(comments) != 1 {
		t.Fatalf("expected 1 creation comment, got %d (err %v)", len(comments), err)
	}
	if !strings.Contains(comments[0].Content, "mention://agent/"+agentB) {
		t.Fatalf("mention URI should be preserved verbatim, got: %q", comments[0].Content)
	}
}

// TestCreateNoCommentWithoutDescription: a goal without an instruction gets
// no creation comment (nothing to record).
func TestCreateNoCommentWithoutDescription(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "writer")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{
		Title:        "no instruction",
		DomainID:     domID,
		AssigneeType: "agent",
		AssigneeID:   agentA,
		Status:       "active",
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	comments, err := NewCommentService(st, nil).List(ctx, g.ID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("no description → no creation comment, got %d", len(comments))
	}
}

// TestAgentCommentAutoThreadsToTrigger: an agent comment made inside a run
// (run_id carried from AGENTWORK_RUN_ID) automatically replies to the
// comment that triggered that run — mention → run → reply chains without
// any agent cooperation (platform mechanism, decision 4-4 spirit).
func TestAgentCommentAutoThreadsToTrigger(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	enqueueFirst(t, rs, g)

	trigger, err := cs.Create(ctx, Comment{GoalID: g.ID, Content: "[@B](mention://agent/" + agentB + ") 请审查"})
	if err != nil {
		t.Fatalf("trigger comment: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	var guest *Run
	for i := range runs {
		if runs[i].AgentID == agentB {
			guest = &runs[i]
		}
	}
	if guest == nil || guest.TriggerCommentID != trigger.ID {
		t.Fatalf("guest run must carry the trigger comment, got %+v", guest)
	}

	// The agent replies from inside its run — no parent knowledge needed.
	reply, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentB, Content: "审查结论：通过", RunID: guest.ID})
	if err != nil {
		t.Fatalf("agent reply: %v", err)
	}
	if reply.ParentID != trigger.ID {
		t.Fatalf("agent reply must auto-thread to the trigger comment, got parent=%q want %q", reply.ParentID, trigger.ID)
	}
}
