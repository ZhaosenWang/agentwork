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
		Title:         "add string utils",
		Description:   "作为 dev-team leader：给 test-repo 添加 string_utils.py + 对应测试",
		DomainID:      domID,
		AssigneeType:  "agent",
		AssigneeID:    agentA,
		Status:        "active",
		CreatedByType: "human",
		CreatedByID:   "ui",
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
	// P0-2 (决策 6-15②): creation births exactly the ASSIGNEE's owner run
	// in-tx; the mention in the description must NOT dispatch a guest run on
	// the mentioned agent.
	if n != 1 {
		t.Fatalf("creation must birth exactly the owner run, got %d", n)
	}
	var mentioned int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=?`, g.ID, agentB).Scan(&mentioned); err != nil {
		t.Fatalf("count mentioned-agent runs: %v", err)
	}
	if mentioned != 0 {
		t.Fatalf("the description's mention must stay inert, got %d runs on the mentioned agent", mentioned)
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

// TestCreateNoDispatchIsPureComment: the pure-Comment path (comment_goal's
// contract, 决策 5-2) persists the comment but mentions in it NEVER trigger
// runs — even for the goal's OWNER, whose Create-path comments would
// dispatch a consult. "Saying" must not become "asking".
func TestCreateNoDispatchIsPureComment(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	agentB := seedAgent(t, st, "other")
	domID := seedDomain(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})

	// The OWNER posts a pure comment carrying a mention URI.
	if _, err := cs.CreateNoDispatch(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "[@other](mention://agent/" + agentB + ") 顺便看看", RunID: "",
	}); err != nil {
		t.Fatalf("pure comment: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='consult'`, g.ID).Scan(&n); err != nil {
		t.Fatalf("count consult runs: %v", err)
	}
	if n != 0 {
		// P0-2: the owner's birth run exists (role=owner) — the pure comment
		// must not have added a GUEST run for the mention.
		t.Fatalf("pure comment must not dispatch a guest run, got %d", n)
	}

	// The dispatch path still works for the same content (the Consult
	// mechanism, used by consult_agent / human mentions).
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "[@other](mention://agent/" + agentB + ") 问一下", RunID: "",
	}); err != nil {
		t.Fatalf("dispatch comment: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=?`, g.ID, agentB).Scan(&n); err != nil {
		t.Fatalf("count guest runs: %v", err)
	}
	if n != 1 {
		t.Fatalf("Create must still dispatch the owner's mention, got %d guest runs", n)
	}
	_ = rs
}

// TestRunReportThreadsToTrigger: the platform's automatic run-report comment
// (the guest's answer) threads to the mention that pulled the guest in — the
// feed reads "mention → run → answer" without the agent cooperating
// (Collaboration.md §12: the answer goes back to the requester).
func TestRunReportThreadsToTrigger(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	trigger, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "human", AuthorID: "ui", Content: "[@B](mention://agent/" + agentB + ") 请审查"})
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
	if guest == nil {
		t.Fatalf("mention must enqueue a guest run for B")
	}

	// The guest finishes WITHOUT commenting itself — the platform lands the
	// run's report; it must reply to the mention.
	if err := rs.Finish(ctx, guest.ID, "completed", "审查结论：通过"); err != nil {
		t.Fatalf("finish guest run: %v", err)
	}
	var parentID, content string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(parent_id,''), content FROM comment WHERE run_id=?`, guest.ID).Scan(&parentID, &content); err != nil {
		t.Fatalf("load run-report comment: %v", err)
	}
	if parentID != trigger.ID {
		t.Fatalf("run report must thread to the trigger comment, got parent=%q want %q", parentID, trigger.ID)
	}
	if content != "审查结论：通过" {
		t.Fatalf("run report content = %q", content)
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

	trigger, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "human", AuthorID: "ui", Content: "[@B](mention://agent/" + agentB + ") 请审查"})
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
