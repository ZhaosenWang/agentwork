package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// ── The engineered context system (决策 6-22) ──
//
// Every prompt = the FIXED BLOCK (once per session) + the WAKE LINE (every
// turn). The feed is PULLED, never injected; AGENTWORK.md is retired.

// seedCtx builds the store bits the context builders read.
func seedCtx(t *testing.T) (*Daemon, *store.Store, string, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{st: st}
	rt, err := service.NewRuntimeService(st).Create(context.Background(), service.Runtime{Name: "rt", MachineID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	agentB, err := service.NewAgentService(st, events.NewBus()).Create(context.Background(), service.Agent{Name: "B", RuntimeID: rt.ID})
	if err != nil {
		t.Fatal(err)
	}
	dom, err := service.NewDomainService(st, events.NewBus()).Create(context.Background(), service.Domain{Name: "d", GitURL: "https://example.com/d.git", PolicyText: "测试能过"})
	if err != nil {
		t.Fatal(err)
	}
	gs := service.NewGoalService(st, events.NewBus())
	gs.SetRunService(service.NewRunService(st, events.NewBus()))
	g, err := gs.Create(context.Background(), service.Goal{Title: "g", Description: "desc", AssigneeType: "agent", AssigneeID: agentB.ID, Status: "active", DomainID: dom.ID})
	if err != nil {
		t.Fatal(err)
	}
	return d, st, g.ID, agentB.ID
}

// TestFixedBlockShape: every section is present, materials keep their
// language — the TEAM is NOT part of the prompt anymore (it rides the
// workdir's AGENTS.md, see TestTeamProfile).
func TestFixedBlockShape(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	block := d.buildFixedBlock(ctx, goalID, agentID, "B", "owner", "g", "测试能过", "repo", "")

	for _, want := range []string{
		"# Background & Requirements", "# Goal", "# Who You Are", "# Tools",
		"- Title: g", "- Acceptance policy: 测试能过",
		"agentwork goal comments", "agentwork help",
		"WITHOUT --after", // the no-memory contract
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("fixed block must carry %q, got:\n%s", want, block)
		}
	}
	if strings.Contains(block, "# Team") || strings.Contains(block, "agentwork MCP") {
		t.Fatalf("the prompt must not carry the team block or MCP-era names, got:\n%s", block)
	}
	_ = st
}

// TestFixedBlockRoleContracts: each role gets its own behavioral contract —
// the reviewer is told review-only, the owner the dispatch rules.
func TestFixedBlockRoleContracts(t *testing.T) {
	d, _, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	owner := d.buildFixedBlock(ctx, goalID, agentID, "B", "owner", "g", "", "repo", "")
	for _, want := range []string{"only what you post as a", "never write ids", "JUDGED, not declared", "agentwork subgoal create --title T --assignee <agent-id>"} {
		if !strings.Contains(owner, want) {
			t.Fatalf("the owner contract must carry %q", want)
		}
	}
	reviewer := d.buildFixedBlock(ctx, goalID, agentID, "B", "review", "g", "", "repo", "")
	if !strings.Contains(reviewer, "REVIEW ONLY") || !strings.Contains(reviewer, "never do the work") {
		t.Fatalf("the reviewer contract must be review-only, got:\n%s", reviewer)
	}
	sub := d.buildFixedBlock(ctx, goalID, agentID, "B", "subgoal", "g", "", "repo", "")
	if !strings.Contains(sub, "To communicate results, use") {
		t.Fatalf("the subgoal contract must direct results to goal comment, got:\n%s", sub)
	}
}

// TestTeamProfile: the squad context rides buildTeamProfile — the leader
// gets the operating protocol (CLI commands, reviewer rule) and the roster
// carries member skills; a member sees the roster + playbook; a solo goal
// gets nothing.
func TestTeamProfile(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	d.squadSvc = service.NewSquadService(st, events.NewBus())

	// A skill library entry + the leader's own selection (the roster shows
	// member skills; the leader's line carries its own).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk-1','frontend','','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE agent SET skills='["sk-1"]' WHERE id=?`, agentID); err != nil {
		t.Fatal(err)
	}
	// The goal becomes squad-assigned; the squad has the leader + coder.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO squad (id,name,leader_id,instructions,created_at) VALUES ('sq-1','team',?, 'playbook says X', '')`, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO squad_member (id,squad_id,member_type,member_id,role,created_at) VALUES ('sm-1','sq-1','agent',?,'implementer','')`, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET assignee_type='squad', assignee_id='sq-1' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}

	leader := d.buildTeamProfile(ctx, goalID, agentID)
	for _, want := range []string{"Squad Operating Protocol", "agentwork subgoal create --title T --assignee <agent-id>", "REVIEWER-ONLY", "playbook says X", "skills: frontend"} {
		if !strings.Contains(leader, want) {
			t.Fatalf("the leader profile must carry %q, got:\n%s", want, leader)
		}
	}
	if strings.Contains(leader, "agentwork_create_sub_goal") {
		t.Fatalf("the leader profile must not carry MCP-era tool names, got:\n%s", leader)
	}
}

// TestWakeLineShapes: ONE unified shape — "You were mentioned by <who>
// (comment <id>):" + content; no wrapper header; the anchor is optional.
func TestWakeLineShapes(t *testing.T) {
	wl := buildWakeLine("c1", "openagent-pm", "> 你觉得这个方案怎么样？")
	if strings.Contains(wl, "## Why you were woken") || !strings.Contains(wl, "You were mentioned by openagent-pm (comment c1):") ||
		!strings.Contains(wl, "你觉得这个方案怎么样？") {
		t.Fatalf("mention wake line:\n%s", wl)
	}
	wl = buildWakeLine("", "the platform", "- 1 change(s) ready to integrate — inspect with agentwork_get_change, merge each with agentwork_integrate_change")
	if !strings.Contains(wl, "You were mentioned by the platform:") || !strings.Contains(wl, "1 change(s) ready to integrate") {
		t.Fatalf("platform wake line:\n%s", wl)
	}
	wl = buildWakeLine("rpt-1", "the platform", "Review the goal's outcome — inspect the diff and the feed.")
	if !strings.Contains(wl, "You were mentioned by the platform (comment rpt-1):") {
		t.Fatalf("review wake line:\n%s", wl)
	}
	wl = buildWakeLine("", "the user", "看看 README 有没有问题")
	if !strings.Contains(wl, "You were mentioned by the user:") {
		t.Fatalf("assignment wake line:\n%s", wl)
	}
}

// TestHandoffPromptCarriesPreviousOwnerReport is the regression for the
// "agent 像没有记忆一样" handoff bug — the cross-agent memory gap. A new owner's
// ACP session cannot load the previous owner's session (different persona +
// agent-keyed persistent workdir). Cross-agent memory relies on ACP session
// resume + the agent pulling the feed (`goal comments`), NOT on the platform
// injecting result_summary text (决策 4-4 revised). The handoff branch must
// carry the handoff note but must NOT inject the previous owner's report.
func TestHandoffPromptDoesNotInjectPreviousOwnerReport(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()

	// A previous owner run of this goal completed with a substantive report.
	// The new owner (agentID here — seedCtx assigns the goal to it; we simulate
	// a prior owner by inserting an older run under a different agent row that
	// shares the same runtime).
	var rtID string
	if err := st.DB().QueryRowContext(ctx, `SELECT id FROM runtime LIMIT 1`).Scan(&rtID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	prevAgent, err := service.NewAgentService(st, events.NewBus()).Create(ctx, service.Agent{Name: "PrevOwner", RuntimeID: rtID})
	if err != nil {
		t.Fatalf("seed prev owner: %v", err)
	}
	// Seed a runtime row for the prev agent's runtime FK (seedCtx created rt
	// with machine m1; reuse it).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES ('run-prev',?,?,?,?,?,?,?,?,?,?,?)`,
		goalID, prevAgent.ID, "worker", "worker", "completed", "owner", 1,
		"已经把登录页改好了，剩下的注册页还没动，token 放在 .env 里", "2026-08-17T10:00:00Z", "2026-08-17T09:00:00Z", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert prev run: %v", err)
	}

	// A handoff wake: the new owner's prompt carries the handoff note. The
	// previous owner's report is NOT injected (决策 4-4 revised) — cross-agent
	// memory relies on ACP session resume + the agent pulling the feed.
	q := &service.ClaimedRow{RunID: "run-new", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g",
		handoff: "你来接手注册页",
	})
	if !strings.Contains(prompt, "你来接手注册页") {
		t.Fatalf("handoff prompt must carry the handoff note, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Previous owner's last report") {
		t.Fatalf("handoff prompt must NOT inject result_summary text (决策 4-4 revised), got:\n%s", prompt)
	}
}

// TestHandoffPromptNoReportWhenNoPriorRun: a handoff with no previous owner
// report (first handoff ever, or the prior run was cancelled mid-flight with
// no summary) must not inject an empty "Previous owner's last report" block —
// that would confuse the new owner with a blank citation.
func TestHandoffPromptNoReportWhenNoPriorRun(t *testing.T) {
	d, _, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	q := &service.ClaimedRow{RunID: "run-new", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g",
		handoff: "first handoff, no prior work",
	})
	if strings.Contains(prompt, "Previous owner's last report") {
		t.Fatalf("handoff prompt must NOT inject an empty report block when there is no prior summary, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "first handoff, no prior work") {
		t.Fatalf("handoff prompt must still carry the handoff note, got:\n%s", prompt)
	}
}

// TestHandoffPromptSkipsCancelledRunSummary: result_summary injection was
// RETIRED (决策 4-4 revised). The handoff prompt must NOT inject any
// result_summary — neither the cancelled run's platform noise nor the older
// completed run's report. Cross-agent memory relies on ACP session resume +
// the agent pulling the feed.
func TestHandoffPromptSkipsCancelledRunSummary(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	var rtID string
	if err := st.DB().QueryRowContext(ctx, `SELECT id FROM runtime LIMIT 1`).Scan(&rtID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	prevAgent, err := service.NewAgentService(st, events.NewBus()).Create(ctx, service.Agent{Name: "PrevOwner", RuntimeID: rtID})
	if err != nil {
		t.Fatalf("seed prev owner: %v", err)
	}
	// An older COMPLETED run with the real report.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES ('run-done',?,?,?,?,?,?,?,?,?,?,?)`,
		goalID, prevAgent.ID, "worker", "worker", "completed", "owner", 1,
		"登录页已完成，注册页待做", "2026-08-17T10:00:00Z", "2026-08-17T09:00:00Z", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert completed run: %v", err)
	}
	// A NEWER cancelled run (handoff cut) — its summary is platform noise.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES ('run-cut',?,?,?,?,?,?,?,?,?,?,?)`,
		goalID, prevAgent.ID, "worker", "worker", "cancelled", "owner", 2,
		"cancelled by platform", "2026-08-17T11:00:00Z", "2026-08-17T10:30:00Z", "2026-08-17T10:30:00Z"); err != nil {
		t.Fatalf("insert cancelled run: %v", err)
	}
	q := &service.ClaimedRow{RunID: "run-new", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g",
		handoff: "接手注册页",
	})
	if strings.Contains(prompt, "cancelled by platform") {
		t.Fatalf("handoff prompt must NOT inject the cancelled run's platform-noise summary, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "登录页已完成，注册页待做") {
		t.Fatalf("handoff prompt must NOT inject result_summary text (决策 4-4 revised), got:\n%s", prompt)
	}
}

// TestRejectPromptWakeIsNotHandoff is the 决策 7-2 regression: a reject
// successor run must NOT take the handoff wake branch (which injects
// "Previous owner's last report" — wrong label, since the owner was never
// changed, only paused for review). Reject has its own isReject branch:
// wake line carries the reject reason, memory label is "Your previous round
// was REJECTED". The precondition: goal's latest gate_decision = reject,
// run has no trigger/note/handoff but a wake_anchor pointing at the reject
// comment.
func TestRejectPromptWakeIsNotHandoff(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()

	// The owner's previous completed run (the rejected work) — the memory
	// source. Its report is what the owner must continue from.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES ('run-rej',?,?,?,?,?,?,?,?,?,?,?)`,
		goalID, agentID, "worker", "worker", "completed", "owner", 1,
		"登录页改了一半，注册页还没动", "2026-08-17T10:00:00Z", "2026-08-17T09:00:00Z", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert rejected run: %v", err)
	}

	// The reject comment (human-authored) — this is the wake anchor.
	rejectCommentID := "cmt-rej"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
		rejectCommentID, goalID, "human", "驳回：方向不对，把 X 改成 Y 再看", "2026-08-17T11:00:00Z"); err != nil {
		t.Fatalf("insert reject comment: %v", err)
	}

	// The gate_decision row — the authoritative isReject signal. Without it
	// assemblePrompt falls through to the default assignment branch.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO gate_decision (id,goal_id,run_id,gate_rule,decision,reason,decided_by,decided_at,review_duration)
		 VALUES ('gd-1',?,?,?,?,?,?,?,?)`,
		goalID, "run-rej", "merge", "reject", "方向不对，把 X 改成 Y 再看", "human", "2026-08-17T11:00:00Z", 120); err != nil {
		t.Fatalf("insert gate_decision: %v", err)
	}

	// The reject successor run: no trigger, no wake_note, no handoff (reject
	// does not write it), wake_anchor = reject comment. This is exactly the
	// shape ResolveReview produces.
	q := &service.ClaimedRow{RunID: "run-new", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g",
		wakeAnchorID: rejectCommentID,
		// triggerCommentID, wakeNote, handoff all "" — the reject signature
	})

	// The reject reason must reach the owner via the wake line.
	if !strings.Contains(prompt, "驳回：方向不对，把 X 改成 Y 再看") {
		t.Fatalf("reject prompt must carry the reject reason in the wake line, got:\n%s", prompt)
	}
	// result_summary injection was RETIRED (决策 4-4 revised) — the reject
	// prompt must NOT inject the previous run's report text. Cross-round
	// memory relies on ACP session resume + the agent pulling the feed.
	if strings.Contains(prompt, "Your previous round was REJECTED") {
		t.Fatalf("reject prompt must NOT inject result_summary text (决策 4-4 revised), got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Previous owner's last report") {
		t.Fatalf("reject prompt must NOT use the handoff memory label — the owner was never changed, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "登录页改了一半，注册页还没动") {
		t.Fatalf("reject prompt must NOT inject the owner's previous result_summary, got:\n%s", prompt)
	}
}

// TestRejectPromptFallsThroughWithoutGateDecision: if there is NO
// gate_decision = reject (e.g. a bare owner spawn that happens to have no
// trigger/note/handoff/anchor), the prompt must NOT misfire as reject — it
// falls through to the default assignment branch. This guards against
// isReject false-positives from the "all-empty" signature alone.
func TestRejectPromptFallsThroughWithoutGateDecision(t *testing.T) {
	d, _, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	// No gate_decision, no reject comment — a bare owner spawn (first
	// assignment shape, but with a wake_anchor). Must NOT take isReject.
	q := &service.ClaimedRow{RunID: "run-bare", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g", desc: "do the thing",
		// All empty except desc — default assignment branch fires.
	})
	if strings.Contains(prompt, "REJECTED") {
		t.Fatalf("a non-reject owner spawn must NOT take the isReject branch, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "do the thing") {
		t.Fatalf("bare spawn should fall through to default (desc as wake), got:\n%s", prompt)
	}
}

// TestHandoffPromptUnaffectedByRejectChange: the handoff path must still
// work after the reject refactor — handoff_note non-empty + owner role +
// no trigger/note takes the handoff branch, NOT isReject (gate_decision
// absent or not reject). This guards the mutual-exclusivity: reject clears
// handoff_note, so a non-empty handoff_note means a real handoff.
func TestHandoffPromptUnaffectedByRejectChange(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	// A previous owner completed run (handoff memory source).
	var rtID string
	_ = st.DB().QueryRowContext(ctx, `SELECT id FROM runtime LIMIT 1`).Scan(&rtID)
	prevAgent, _ := service.NewAgentService(st, events.NewBus()).Create(ctx, service.Agent{Name: "Prev", RuntimeID: rtID})
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES ('run-prev',?,?,?,?,?,?,?,?,?,?,?)`,
		goalID, prevAgent.ID, "worker", "worker", "completed", "owner", 1,
		"前一任的活干到这", "2026-08-17T10:00:00Z", "2026-08-17T09:00:00Z", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert prev run: %v", err)
	}
	// NO gate_decision — this is a real handoff, not a reject.
	q := &service.ClaimedRow{RunID: "run-h", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g",
		handoff: "你来接手",
	})
	if strings.Contains(prompt, "Previous owner's last report") {
		t.Fatalf("handoff prompt must NOT inject result_summary text (决策 4-4 revised), got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Your previous round was REJECTED") {
		t.Fatalf("handoff prompt must NOT take the reject branch, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "你来接手") {
		t.Fatalf("handoff prompt must carry the handoff note, got:\n%s", prompt)
	}
}

// TestConsultStatusPicksAgentCommentByRunID (决策 4-4 revised + 5-8): the
// guest's consult answer is the agent's own `goal comment` (carrying
// run_id=guest_run_id). consultStatus must join on comment.run_id (not the
// retired response_comment_id) and pick the LAST agent comment when several
// exist on the same run — a bare LEFT JOIN would produce a cartesian product
// (one row per comment → duplicate consult entries in the prompt).
func TestConsultStatusPicksAgentCommentByRunID(t *testing.T) {
	d, st, goalID, ownerID := seedCtx(t)
	ctx := context.Background()

	// Seed a second agent (the consult target).
	rt, _ := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt2", MachineID: "m2"})
	expert, _ := service.NewAgentService(st, events.NewBus()).Create(ctx, service.Agent{Name: "Expert", RuntimeID: rt.ID})

	// The owner's consult trigger comment.
	triggerID := "cmt-trigger"
	now := "2026-09-09T10:00:00Z"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,content,created_at) VALUES (?,?,?,?,?,?)`,
		triggerID, goalID, "agent", ownerID, "[@Expert](mention://agent/"+expert.ID+") how?", now); err != nil {
		t.Fatal(err)
	}

	// The guest consult run (role=consult, completed).
	guestRunID := "run-guest"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,trigger_comment_id,finished_at,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		guestRunID, goalID, expert.ID, "worker", "worker", "completed", "consult", 1, triggerID, "2026-09-09T10:05:00Z", "2026-09-09T10:01:00Z", "2026-09-09T10:01:00Z"); err != nil {
		t.Fatal(err)
	}

	// The consult_request row (requester=owner, guest=expert's run).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO consult_request (id,goal_id,requester_agent_id,requester_run_id,target_agent_id,trigger_comment_id,guest_run_id,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		"cr-1", goalID, ownerID, "owner-run-1", expert.ID, triggerID, guestRunID, now); err != nil {
		t.Fatal(err)
	}

	// The owner's previous run (the "since your last turn" scope filter).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,finished_at,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"owner-run-1", goalID, ownerID, "worker", "worker", "completed", "owner", 1, "2026-09-09T09:00:00Z", "2026-09-09T08:00:00Z", "2026-09-09T08:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// Agent posts TWO comments on the guest run — a discussion aside, then
	// the final answer. Both carry run_id=guest_run_id (as the RPC handler
	// fills from the token).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,?,?,?,?,?,?)`,
		"cmt-aside", goalID, "agent", expert.ID, triggerID, "let me think about this", "2026-09-09T10:02:00Z", guestRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,?,?,?,?,?,?)`,
		"cmt-answer", goalID, "agent", expert.ID, triggerID, "the answer is 42", "2026-09-09T10:04:00Z", guestRunID); err != nil {
		t.Fatal(err)
	}

	// The current owner run (the one calling consultStatus).
	ownerRunID := "owner-run-2"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,finished_at,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		ownerRunID, goalID, ownerID, "worker", "worker", "completed", "owner", 1, "2026-09-09T10:10:00Z", "2026-09-09T10:06:00Z", "2026-09-09T10:06:00Z"); err != nil {
		t.Fatal(err)
	}

	// consultStatus must return exactly ONE consult entry (not two from the
	// two comments), and the answer must be the LAST comment ("the answer is
	// 42"), not the aside ("let me think about this").
	status := d.consultStatus(ctx, goalID, ownerID, ownerRunID)
	if !strings.Contains(status, "the answer is 42") {
		t.Fatalf("consultStatus must carry the guest's final answer, got:\n%s", status)
	}
	if strings.Contains(status, "let me think about this") {
		t.Fatalf("consultStatus must pick the LAST agent comment, not the aside, got:\n%s", status)
	}
	// The consult entry must appear exactly once (no cartesian-product dup).
	if c := strings.Count(status, "how?"); c != 1 {
		t.Fatalf("consultStatus must render the consult once, got %d occurrences in:\n%s", c, status)
	}
}
