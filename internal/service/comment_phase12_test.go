package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// captureEvents returns a func that drains the buffered capture of every
// publish on the given topic since the last drain. The bus fires handlers in
// goroutines, so a plain int counter races the assertion; the channel +
// drain-with-deadline form is the same pattern the notify tests use.
func captureEvents(bus *events.Bus, topic string) func(deadline time.Duration) int {
	var mu sync.Mutex
	var n int
	bus.Subscribe(topic, func(_ context.Context, _ events.Event) {
		mu.Lock()
		n++
		mu.Unlock()
	})
	return func(deadline time.Duration) int {
		deadlineT := time.Now().Add(deadline)
		for time.Now().Before(deadlineT) {
			mu.Lock()
			c := n
			mu.Unlock()
			if c > 0 {
				return c
			}
			time.Sleep(2 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// TestAgentAskCommentPublishesAgentQuestionEvent (决策 7-3): an agent's
// `goal comment --ask` (AskHuman=true, author=agent) publishes the dedicated
// comment:agent_question event so notify pushes a Feishu card. A plain agent
// comment (AskHuman=false) must NOT fire it — the event is exclusive to a
// real question, never every comment. A human-authored AskHuman comment is a
// no-op (a human cannot ask themselves).
func TestAgentAskCommentPublishesAgentQuestionEvent(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	enqueueFirst(t, rs, g)

	askQ := captureEvents(gs.bus, "comment:agent_question")

	// An --ask question from the owner agent → fires the dedicated event.
	ask := Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "请问这个接口的入参用 JSON 还是 form？", AskHuman: true,
	}
	if _, err := cs.Create(ctx, ask); err != nil {
		t.Fatalf("ask comment: %v", err)
	}
	if got := askQ(time.Second); got != 1 {
		t.Fatalf("agent --ask must publish comment:agent_question once, got %d", got)
	}

	// A plain agent comment must NOT fire the question event.
	askQ2 := captureEvents(gs.bus, "comment:agent_question")
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "进度过半", RunID: "",
	}); err != nil {
		t.Fatalf("plain comment: %v", err)
	}
	if got := askQ2(300 * time.Millisecond); got != 0 {
		t.Fatalf("plain agent comment must NOT fire comment:agent_question, got %d", got)
	}

	// The ask_human flag persists (the web renders the ❓ badge from it).
	var askHuman int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT ask_human FROM comment WHERE content=?`, ask.Content).Scan(&askHuman); err != nil {
		t.Fatalf("load persisted ask_human: %v", err)
	}
	if askHuman != 1 {
		t.Fatalf("ask_human must persist as 1, got %d", askHuman)
	}
}

// TestHumanAskCommentDoesNotFireAgentQuestion: a human-authored comment with
// AskHuman set is a no-op — the question event is agent→human only. (The web
// never sends ask_human for human comments, but the service defends in depth.)
func TestHumanAskCommentDoesNotFireAgentQuestion(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	enqueueFirst(t, rs, g)
	_ = st

	askQ := captureEvents(gs.bus, "comment:agent_question")
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		Content: "自问自答", AskHuman: true,
	}); err != nil {
		t.Fatalf("human ask comment: %v", err)
	}
	if got := askQ(300 * time.Millisecond); got != 0 {
		t.Fatalf("human-authored AskHuman must NOT fire comment:agent_question, got %d", got)
	}
}

// TestHumanReplyToAgentCommentWakesOwner (决策 7-3 core): a HUMAN reply whose
// parent_id points at an AGENT comment wakes the goal's current OWNER as
// role='owner' — NOT a consult guest run (fresh empty workdir, no context).
// The real-world shape: the owner's first run already COMPLETED (the agent
// posted its --ask question and ended), so no owner run is pending to coalesce
// into — the reply enqueues a fresh owner run carrying the reply as its
// trigger. The web auto-inserts a mention link in replies; the owner wake must
// happen WITHOUT a consult dispatch on the co-present mention (double-trigger
// guard).
func TestHumanReplyToAgentCommentWakesOwner(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	first := enqueueFirst(t, rs, g)

	// The agent's prior comment in the feed — the reply target.
	agentCmt, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "用 JSON 吗？", RunID: first.ID,
	})
	if err != nil {
		t.Fatalf("agent comment: %v", err)
	}
	// Finish the owner's first run so no owner run is pending to coalesce
	// into — the reply enqueues a FRESH owner run. (The goal may go terminal;
	// the reply reopens it, which is the realistic "agent asked, task ended,
	// human replies" shape.)
	if err := rs.Finish(ctx, first.ID, "completed", "asked the question"); err != nil {
		t.Fatalf("finish first run: %v", err)
	}

	// A human reply threading to the agent comment. The web would also insert
	// a mention://agent link — we carry one to prove the double-trigger guard.
	reply, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		ParentID: agentCmt.ID,
		Content: "[@owner](mention://agent/" + agentA + ") 用 JSON",
	})
	if err != nil {
		t.Fatalf("human reply: %v", err)
	}
	if reply.ParentID != agentCmt.ID {
		t.Fatalf("reply parent_id = %q want %q", reply.ParentID, agentCmt.ID)
	}

	// The wake run is the LATEST run on the owner agent — role='owner', with
	// the reply comment as its trigger (so the wake line carries the reply).
	var role, agentID, trigger string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT role, agent_id, trigger_comment_id FROM run WHERE goal_id=? AND trigger_comment_id=? ORDER BY created_at DESC LIMIT 1`, g.ID, reply.ID).
		Scan(&role, &agentID, &trigger); err != nil {
		t.Fatalf("no owner wake run enqueued on the reply (trigger=%s): %v", reply.ID, err)
	}
	if role != "owner" {
		t.Fatalf("human→agent reply wake run role = %q, want 'owner' (not consult)", role)
	}
	if agentID != agentA {
		t.Fatalf("wake run agent = %q, want owner %q", agentID, agentA)
	}
	if trigger != reply.ID {
		t.Fatalf("wake run trigger = %q, want reply comment %q", trigger, reply.ID)
	}
	// No consult-shaped run exists for this goal — the double-trigger guard
	// (the co-present mention link must not also enqueue a consult run).
	var consultN int
	_ = st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='consult'`, g.ID).Scan(&consultN)
	if consultN != 0 {
		t.Fatalf("human→agent reply must NOT enqueue a consult run, got %d", consultN)
	}
}

// TestHumanReplyToHumanCommentIsNotOwnerWake: the parent_id→owner routing
// fires ONLY when the parent is an AGENT comment. A top-level human comment
// that carries a mention takes the original mention dispatch path — a consult
// run as before. This guards the routing's specificity.
func TestHumanReplyToHumanCommentIsNotOwnerWake(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	agentB := seedAgent(t, st, "consultant")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	enqueueFirst(t, rs, g)

	// A human top-level comment (no parent) that mentions agentB — the
	// classic human consult. Must dispatch a consult run for agentB.
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		Content: "[@consultant](mention://agent/" + agentB + ") 看一下",
	}); err != nil {
		t.Fatalf("human mention comment: %v", err)
	}
	var consultN int
	_ = st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='consult' AND agent_id=?`, g.ID, agentB).Scan(&consultN)
	if consultN != 1 {
		t.Fatalf("human→agent mention (no parent) must dispatch a consult run, got %d", consultN)
	}
}

// TestHumanReplyToAgentCommentReopensTerminalGoal: a human reply to an agent
// comment on a TERMINAL goal reopens it and then wakes the owner — "this task
// is not over". Without the reopen, the terminal status guard would swallow
// the reply and the owner never wakes (the original bug: task ended, agent
// had no context).
func TestHumanReplyToAgentCommentReopensTerminalGoal(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	first := enqueueFirst(t, rs, g)

	// The agent's prior comment.
	agentCmt, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "做好了", RunID: first.ID,
	})
	if err != nil {
		t.Fatalf("agent comment: %v", err)
	}
	// Finish the run + force the goal terminal (done) — the reopen is the point.
	if err := rs.Finish(ctx, first.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatalf("force done: %v", err)
	}

	// Human replies to the agent comment on the terminal goal.
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		ParentID: agentCmt.ID,
		Content: "再改一下",
	}); err != nil {
		t.Fatalf("human reply on terminal: %v", err)
	}
	var status string
	_ = st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, g.ID).Scan(&status)
	if status != "active" {
		t.Fatalf("terminal goal must reopen on human→agent reply, got status %q", status)
	}
	// An owner wake run was enqueued (role='owner', trigger = the reply).
	var n int
	_ = st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='owner' AND status='queued'`, g.ID).Scan(&n)
	if n == 0 {
		t.Fatalf("reopened goal must enqueue the owner wake run")
	}
}

// TestHumanReplyToAgentCommentOnSquadGoalWakesLeader: a squad-assigned goal's
// owner is its leader; a human reply to an agent (member) comment must wake
// the LEADER as role='owner', not the comment's author (the member).
func TestHumanReplyToAgentCommentOnSquadGoalWakesLeader(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")
	member := seedAgent(t, st, "member")
	domID := seedDomain(t, st)

	// Build a squad: leader + member.
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "team", LeaderID: leader, Instructions: ""})
	if err != nil {
		t.Fatalf("seed squad: %v", err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", member, "implementer"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "squad", AssigneeID: sq.ID, Status: "active", DomainID: domID})
	// The squad's first run is on the leader (owner). Finish it so the reply
	// enqueues a fresh owner run rather than coalescing.
	first := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, first.ID, "completed", "leader round done"); err != nil {
		t.Fatalf("finish leader run: %v", err)
	}

	// A member's comment in the feed — the reply target.
	memberCmt, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: member,
		Content: "需要确认", RunID: "",
	})
	if err != nil {
		t.Fatalf("member comment: %v", err)
	}

	// Human replies to the member comment → owner wake on the LEADER.
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		ParentID: memberCmt.ID,
		Content: "确认通过",
	}); err != nil {
		t.Fatalf("human reply to member: %v", err)
	}
	var role, agentID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT role, agent_id FROM run WHERE goal_id=? AND status='queued' ORDER BY created_at DESC LIMIT 1`, g.ID).Scan(&role, &agentID); err != nil {
		t.Fatalf("no queued wake run: %v", err)
	}
	if role != "owner" {
		t.Fatalf("squad reply wake role = %q, want 'owner'", role)
	}
	if agentID != leader {
		t.Fatalf("squad reply wake agent = %q, want leader %q (not member %q)", agentID, leader, member)
	}
}

// _ keeps the strings import bound when a future edit drops a Contains call.
var _ = strings.Contains

// TestAskHumanHoldsGoalActive (决策 7-3 延伸): an owner run that ends while
// the agent has an UNANSWERED --ask question to the human must NOT park in
// review or promote to done — the owner is mid-flight waiting for the human's
// reply (which wakes it via parent_id routing). Without this guard the goal
// races to review/done before the human even sees the Feishu card, and the
// reply then lands on a terminal goal (the original "agent asked, task ended"
// bug at the reconcile layer, not the routing layer).
func TestAskHumanHoldsGoalActive(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st) // frozen, no gates, medium → completed run promotes to done
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	ownerRun := enqueueFirst(t, rs, g)

	// The agent asks the human a question mid-run (--ask), then ends its turn.
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "用 JSON 还是 form？", AskHuman: true, RunID: ownerRun.ID,
	}); err != nil {
		t.Fatalf("ask comment: %v", err)
	}
	if err := rs.Finish(ctx, ownerRun.ID, "completed", "asked the question, awaiting reply"); err != nil {
		t.Fatalf("finish owner: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Status != "active" {
		t.Fatalf("an unanswered --ask must hold the goal active (not park in review/done), got %q", g1.Status)
	}
}

// TestAskHumanReleasesOnHumanReply: once the human replies to the --ask
// (parent_id → ask comment), the hold is released — the unanswered-ask count
// drops to 0, so a subsequent owner run ending reaches the gate normally.
// This guards the NOT EXISTS clause: a replied ask must not keep blocking.
func TestAskHumanReleasesOnHumanReply(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	ownerRun := enqueueFirst(t, rs, g)

	// Agent asks, ends its turn → goal holds active (TestAskHumanHoldsGoalActive).
	ask, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "用 JSON 还是 form？", AskHuman: true, RunID: ownerRun.ID,
	})
	if err != nil {
		t.Fatalf("ask comment: %v", err)
	}
	if err := rs.Finish(ctx, ownerRun.ID, "completed", "asked"); err != nil {
		t.Fatalf("finish owner: %v", err)
	}
	if g1, _ := gs.Get(ctx, g.ID); g1.Status != "active" {
		t.Fatalf("precondition: ask must hold active, got %q", g1.Status)
	}

	// Human replies → the ask is now answered (a human reply threads under it).
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		ParentID: ask.ID, Content: "用 JSON",
	}); err != nil {
		t.Fatalf("human reply: %v", err)
	}
	// The unanswered-ask count (the guard's exact SQL) must be 0 now.
	var pendingAsk int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment ask
		 WHERE ask.goal_id=? AND ask.ask_human=1 AND ask.author_type='agent'
		   AND NOT EXISTS (SELECT 1 FROM comment rep WHERE rep.parent_id=ask.id AND rep.author_type='human')`,
		g.ID).Scan(&pendingAsk); err != nil {
		t.Fatalf("count pending ask: %v", err)
	}
	if pendingAsk != 0 {
		t.Fatalf("a replied ask must no longer be pending, got %d", pendingAsk)
	}
	// Enqueue a fresh owner round (the reply-routing path would have done this)
	// and finish it — with the ask answered, the gate fires (done, no gates).
	wake := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, wake.ID, "completed", "done with JSON"); err != nil {
		t.Fatalf("finish wake run: %v", err)
	}
	g2, _ := gs.Get(ctx, g.ID)
	if g2.Status != "done" {
		t.Fatalf("after the ask is answered, the next owner completion must reach the gate (done, no gates), got %q", g2.Status)
	}
}

// TestAskHumanAnsweredDoesNotHold: an --ask that already has a human reply
// must NOT hold the gate — the owner run ending after the reply was answered
// reaches the gate normally. Guards against counting ALL ask_human comments
// regardless of reply state (the NOT EXISTS clause).
func TestAskHumanAnsweredDoesNotHold(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	ownerRun := enqueueFirst(t, rs, g)
	_ = st

	// Agent asks, human replies (answers it), THEN the owner ends its turn.
	ask, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "用 JSON？", AskHuman: true, RunID: ownerRun.ID,
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		ParentID: ask.ID, Content: "对",
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if err := rs.Finish(ctx, ownerRun.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Status != "done" {
		t.Fatalf("an ANSWERED ask must not hold the gate — goal should be done (no gates), got %q", g1.Status)
	}
}

// TestNonAskAgentCommentDoesNotHold: a plain agent comment (not --ask) must
// NOT hold the gate — the hold is exclusive to ask_human. Guards against a
// loose ask_human=1 check or a parent_id misread.
func TestNonAskAgentCommentDoesNotHold(t *testing.T) {
	gs, rs, cs, _ := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, gs.st, "owner")
	domID := seedDomain(t, gs.st)
	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	ownerRun := enqueueFirst(t, rs, g)
	// A plain agent comment (AskHuman=false) — a progress note, not a question.
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "进度过半", RunID: ownerRun.ID,
	}); err != nil {
		t.Fatalf("plain comment: %v", err)
	}
	if err := rs.Finish(ctx, ownerRun.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Status != "done" {
		t.Fatalf("a plain agent comment must not hold the gate, got %q (want done)", g1.Status)
	}
}
