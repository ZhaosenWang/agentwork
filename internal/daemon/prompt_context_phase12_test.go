package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/service"
)

// TestOwnerRunTriggeredByHumanReplyWakeLine (决策 7-3): an owner run whose
// trigger comment is a HUMAN reply (the wake path the comment service's
// parent_id→owner routing enqueues) must render the wake line as
// "You were mentioned by the user (comment <id>):" + the reply content —
// NOT as a consult (which would say "the user" but stamp a guest/consult role
// and use a fresh workdir). The owner role here is what carries the session +
// persistent workdir; the wake line is the human's words.
func TestOwnerRunTriggeredByHumanReplyWakeLine(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()

	// The agent's prior comment — the reply target.
	agentCmtID := "cmt-agent"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
		agentCmtID, goalID, "agent", "用 JSON 吗？", "2026-08-17T10:00:00Z"); err != nil {
		t.Fatalf("insert agent comment: %v", err)
	}
	// The human's reply, threading to the agent comment — this is the run's
	// trigger comment (EnqueueForMentionRole stamps trigger_comment_id=reply).
	replyID := "cmt-reply"
	replyText := "用 JSON"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'ui',?,?,?)`,
		replyID, goalID, "human", agentCmtID, replyText, "2026-08-17T10:05:00Z"); err != nil {
		t.Fatalf("insert human reply: %v", err)
	}

	q := &service.ClaimedRow{RunID: "run-wake", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole:               "owner",
		goalTitle:             "g",
		triggerCommentID:      replyID,
		triggerAuthor:         "human",
		triggerAuthorName:     "你",
		triggerCommentContent: replyText,
	})

	// The wake line names "the user" and anchors the reply comment.
	if !strings.Contains(prompt, "You were mentioned by the user (comment "+replyID+"):") {
		t.Fatalf("owner reply wake must name 'the user' + the reply comment id, got:\n%s", prompt)
	}
	// The reply content is quoted in the wake line.
	if !strings.Contains(prompt, "> "+replyText) {
		t.Fatalf("owner reply wake must carry the reply content, got:\n%s", prompt)
	}
	// It must NOT take the consult, review, handoff, or reject branches.
	if strings.Contains(prompt, "Your previous round was REJECTED") {
		t.Fatalf("owner reply wake must NOT take the reject branch, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Previous owner's last report") {
		t.Fatalf("owner reply wake must NOT take the handoff branch, got:\n%s", prompt)
	}
}

// TestOwnerRunHumanReplyWakeIsNotConsult: the same human-reply wake under the
// OWNER role is distinct from the consult wake — a consult run (role=consult)
// triggered by a human mention also says "the user", but the OWNER path keeps
// the persistent session + workdir (the whole point of 决策 7-3: the agent
// must not see "working directory is empty"). This test pins the owner path's
// wake shape so a future refactor that routes human replies back to consult
// is caught: the consult wake uses triggerAuthorName for wakeWho, the owner
// wake hard-codes "the user".
func TestOwnerRunHumanReplyWakeIsNotConsult(t *testing.T) {
	d, _, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	replyID := "cmt-r2"
	// Seed the reply comment so the wake line can be rendered.
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'ui',NULL,?,?)`,
		replyID, goalID, "human", "继续", "2026-08-17T11:00:00Z"); err != nil {
		t.Fatalf("insert reply: %v", err)
	}
	// OWNER role + human trigger → "the user" (owner path).
	ownerPrompt := d.assemblePrompt(ctx,
		&service.ClaimedRow{RunID: "r-o", GoalID: goalID, AgentID: agentID, Attempt: 1},
		promptInputs{
			runRole: "owner", goalTitle: "g",
			triggerCommentID: replyID, triggerAuthor: "human",
			triggerAuthorName: "某agent", triggerCommentContent: "继续",
		})
	if !strings.Contains(ownerPrompt, "You were mentioned by the user") {
		t.Fatalf("owner path wake must say 'the user' regardless of triggerAuthorName, got:\n%s", ownerPrompt)
	}
	// CONSULT role + human trigger → triggerAuthorName (the dispatcher name),
	// NOT the owner path's hard-coded "the user". (In production consultRun is
	// derived as runRole=="consult", so the owner-trigger branch — which
	// requires runRole=="owner" — cannot fire here.)
	consultPrompt := d.assemblePrompt(ctx,
		&service.ClaimedRow{RunID: "r-c", GoalID: goalID, AgentID: agentID, Attempt: 1},
		promptInputs{
			runRole: "consult", goalTitle: "g",
			consultRun: true,
			triggerCommentID: replyID, triggerAuthor: "human",
			triggerAuthorName: "某agent", triggerCommentContent: "看看",
		})
	if !strings.Contains(consultPrompt, "You were mentioned by 某agent") {
		t.Fatalf("consult wake must use the dispatcher name, got:\n%s", consultPrompt)
	}
}

// TestOwnerReplyWakeupCarriesPreviousAsk (决策 7-3 延伸): when the owner is
// woken by a HUMAN reply that threads under the owner's own previous comment
// (the --ask question or a plain report), the prompt must inject that parent
// comment — the owner's own words the human is replying to. Without it the
// reply is unmoored: "你认为呢？" answers nothing the owner can see, and the
// owner drifts (the live failure: PM re-analyzed the goal instead of picking
// up its own test-framework question). This is the agent→human analogue of
// the reject memory (the owner's own previous round, continued).
func TestOwnerReplyWakeupCarriesPreviousAsk(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()

	// The owner's previous --ask question — the reply target.
	askID := "cmt-ask-prev"
	askText := "这个任务用什么测试框架？"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,ask_human) VALUES (?,?,?,'',NULL,?,?,1)`,
		askID, goalID, "agent", askText, "2026-08-17T10:00:00Z"); err != nil {
		t.Fatalf("insert ask: %v", err)
	}
	// The human's reply threading under the ask — this run's trigger.
	replyID := "cmt-reply-2"
	replyText := "你认为呢？"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'ui',?,?,?)`,
		replyID, goalID, "human", askID, replyText, "2026-08-17T10:05:00Z"); err != nil {
		t.Fatalf("insert reply: %v", err)
	}

	q := &service.ClaimedRow{RunID: "run-wake2", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole:               "owner",
		goalTitle:             "g",
		triggerCommentID:      replyID,
		triggerAuthor:         "human",
		triggerCommentContent: replyText,
	})

	// The wake line carries the human's reply.
	if !strings.Contains(prompt, "You were mentioned by the user") || !strings.Contains(prompt, replyText) {
		t.Fatalf("owner reply wake must carry the human's reply, got:\n%s", prompt)
	}
	// The owner's own previous question is injected as context (the reply's
	// parent — what the human is answering). Without this the owner cannot
	// tell which of its questions the reply addresses.
	if !strings.Contains(prompt, "Your previous comment (the human is replying to this)") {
		t.Fatalf("owner reply wake must label the previous-comment context, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, askText) {
		t.Fatalf("owner reply wake must inject the owner's previous ask text, got:\n%s", prompt)
	}
}

// TestOwnerReplyWakeupNoParentIsBare: a human-triggered owner wake whose
// trigger comment has NO agent parent (a top-level human comment, not a
// reply) must NOT inject an empty "previous comment" block — the owner has
// no prior words being replied to. Guards the SQL's parent_id != '' clause
// against a false-positive injection.
func TestOwnerReplyWakeupNoParentIsBare(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	// A top-level human comment (no parent) — the owner's first trigger.
	topID := "cmt-top"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'ui',NULL,?,?)`,
		topID, goalID, "human", "开始干", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert top: %v", err)
	}
	q := &service.ClaimedRow{RunID: "run-top", GoalID: goalID, AgentID: agentID, Attempt: 1}
	prompt := d.assemblePrompt(ctx, q, promptInputs{
		runRole: "owner", goalTitle: "g",
		triggerCommentID: topID, triggerAuthor: "human",
		triggerCommentContent: "开始干",
	})
	if strings.Contains(prompt, "Your previous comment (the human is replying to this)") {
		t.Fatalf("a top-level human trigger (no parent) must NOT inject a previous-comment block, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "开始干") {
		t.Fatalf("the top-level trigger content must still be the wake line, got:\n%s", prompt)
	}
}
