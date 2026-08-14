// Package notify is the IM notification gateway (DESIGN.md §11 M1,
// decision 2-14): it subscribes to milestone bus events and pushes them to
// the owner over the Feishu enterprise-app LONG CONNECTION — the larksuite
// SDK owns authentication, heartbeat, reconnection, and event delivery.
//
// Why long connection and not a custom-bot webhook: the webhook is
// one-way and cannot receive card-button callbacks — the M3 approval
// interactions (approve/reject buttons in Feishu) require the event
// channel the long connection provides. M1 starts with outbound milestone
// pushes on that channel; the card callbacks arrive in M3.
//
// Delivery policy: milestone events only — goal finished / waiting for
// approval / delivered / deliver failed / run cancelled. Run-internal
// events are NEVER pushed; the Web UI is their home.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
)

// Notifier fans milestone events out to the Feishu long connection. The long
// connection itself is managed by the Connector (its SDK client is shared
// here for outbound sends); Notifier only maps events → messages and sends.
//
// Outbound delivery owns its tenant_access_token explicitly (the SDK's token
// manager proved unreliable past expiry — 99991663 on the daily digest after
// a night of uptime). token/tokenExp are guarded by mu.
// approvalCardRec is one sent approval card's patch handle.
type approvalCardRec struct {
	messageID  string
	hadPending bool   // the card showed a "审查中" hint — only then is the opinion patch meaningful
	domainType string // repo|scratch — the processed-card wording branches on it
}

type Notifier struct {
	appID, appSecret string
	client           *lark.Client
	receiveIDType    string     // chat_id | open_id
	receiveID        string     // the owner's chat / open id
	qs               QueryStore // M3: read-only store for card evidence/digest; may be nil
	// send overrides the SDK delivery path (tests inject a mock; nil = the
	// lark client path). Kept unexported on purpose.
	send func(text string) error
	// approvalCards maps goalID → the sent approval card's message_id +
	// whether it carried a "审查中" hint (Option B: the review_ready patch
	// updates the SAME card the human saw at park time; a card without the
	// hint has nothing to patch). In-memory: a restart between park and
	// ready takes the "send fresh" fallback instead. Guarded by mu.
	approvalCards map[string]approvalCardRec
	// updateCardFn overrides the card PATCH (tests inject a mock; nil = the
	// SDK message-update path).
	updateCardFn func(messageID, content string) error

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// New creates a Notifier for a Feishu enterprise app (appID/appSecret).
// receiveIDType is "chat_id" or "open_id"; receiveID is where milestone
// pushes go (the owner's chat with the bot, or a group).
func New(appID, appSecret, receiveIDType, receiveID string) *Notifier {
	if receiveIDType == "" {
		receiveIDType = "chat_id"
	}
	return &Notifier{
		appID: appID, appSecret: appSecret, receiveIDType: receiveIDType, receiveID: receiveID,
		approvalCards: make(map[string]approvalCardRec),
	}
}

// SetQueryStore wires the read-only store (M3): approval cards need the run
// evidence, milestone cards the goal title. Safe to leave nil — the notifier
// falls back to the event payload text.
func (n *Notifier) SetQueryStore(qs QueryStore) { n.qs = qs }

// Subscribe wires the milestone topics. Handlers run in the bus's own
// goroutines (fire-and-forget with panic recovery); each send runs in its
// own goroutine so a slow Feishu API call never blocks the bus.
func (n *Notifier) Subscribe(bus *events.Bus) {
	// Option B (reviewer-first opinions, zero wait): the card fires at park
	// time with a "审查中" hint naming the reviewers; goal:review_ready (the
	// daemon publishes it when this window's review runs are terminal, or
	// the fallback elapsed / no reviewer exists) PATCHES the SAME card to
	// replace the hint with the actual opinions.
	bus.Subscribe("goal:reviewing", n.onGoalReviewing)
	bus.Subscribe("goal:review_ready", n.onGoalReviewReady)
	// A decision made on the WEB never touches the card's buttons — patch
	// the card to its processed state from here (the button-callback path
	// patches through the Connector with its own message id).
	bus.Subscribe("goal:review_resolved", n.onGoalReviewResolved)
	bus.Subscribe("goal:delivered", n.onGoalDelivered)
	bus.Subscribe("goal:deliver_failed", n.onGoalDeliverFailed)
	bus.Subscribe("goal:finished", n.onGoalFinished)
	bus.Subscribe("run:cancelled", n.onRunCancelled)
	// v2 (决策 6-8): a human-owned goal needs its owner's attention — the
	// goal has no agent run to spawn, so the IM push IS the wakeup.
	bus.Subscribe("goal.attention_needed", n.onGoalAttentionNeeded)
}

// onGoalAttentionNeeded pushes the "owner attention" card for a human-owned
// goal (all sub-goals verified / changes ready / recovery needed — no agent
// run exists to act, the human is the owner).
func (n *Notifier) onGoalAttentionNeeded(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	attention, _ := m["attention"].(string)
	if goalID == "" {
		return
	}
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	reason := map[string]string{
		"integration": "有 Change 等待集成",
		"recovery":    "有子任务失败，需要处理",
		"user_action": "需要人工决策",
	}[attention]
	if reason == "" {
		reason = attention
	}
	n.sendMilestoneCard("👤", "purple", "需要你处理", fmt.Sprintf("**%s**  \n`goal %s`  \n%s", title, short(goalID), reason))
}

// NOTE: the published event carries the PUBLISHER's ctx (often an HTTP
// request, cancelled the moment the handler returns) — DB work here would
// fail with "context canceled". Notify is background push; it uses its own
// ctx, not the event's.
func (n *Notifier) onGoalReviewing(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	reason, _ := m["reason"].(string)
	// M3 approval card: the evidence comes from the store (the event only
	// carries the reason). Match the goal's ReviewGoal to carry its run_id —
	// the button callback lands the gate_decision on that exact run.
	if n.qs != nil {
		if goals, err := n.qs.ReviewGoals(ctx); err == nil {
			for _, g := range goals {
				if g.GoalID == goalID {
					if g.Title == "" {
						g.Title = short(goalID)
					}
					// Option B: the card names the reviewers still working and
					// suggests the human MAY wait for them.
					var pending []string
					if reviewers, err := n.qs.PendingReviewers(ctx, goalID); err == nil {
						pending = reviewers
					}
					if card, err := buildReviewCard(g, pending); err == nil {
						n.sendApprovalCard(goalID, card, len(pending) > 0, g.DomainType)
						return
					}
				}
			}
		}
	}
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	n.asyncSend(fmt.Sprintf("🔔 待审批：**%s** 等你决定\n`goal %s`\n%s\n（批准后平台自动合入）", title, short(goalID), reason)) // domain type unknown without the store — repo wording is the default
}

// onGoalReviewReady patches the approval card once the review window closes:
// the "审查中" hint is replaced by the actual opinions (the SAME card — the
// human's context stays intact). If the card was never recorded (a daemon
// restart between park and ready), a fresh card is sent instead.
func (n *Notifier) onGoalReviewReady(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	if n.qs == nil {
		return
	}
	// Locate the review goal (opinions are now in the store's review-run
	// comments).
	var g ReviewGoal
	found := false
	if goals, err := n.qs.ReviewGoals(ctx); err == nil {
		for _, rg := range goals {
			if rg.GoalID == goalID {
				g, found = rg, true
				break
			}
		}
	}
	if !found {
		return // resolved already — the card will show processed via the buttons
	}
	if g.Title == "" {
		g.Title = short(goalID)
	}
	card, err := buildReviewCard(g, nil)
	if err != nil {
		return
	}
	// The daemon's ready can fire BEFORE this notifier's own card send
	// records the message_id (both run on separate bus goroutines, and a
	// no-reviewer window is ready instantly). Wait briefly for the record;
	// only a real miss (restart window) falls back to a fresh card.
	n.mu.Lock()
	rec, ok := n.approvalCards[goalID]
	n.mu.Unlock()
	if !ok {
		deadline := time.Now().Add(reviewReadyPatchWait)
		for time.Now().Before(deadline) {
			time.Sleep(reviewReadyPatchPoll)
			n.mu.Lock()
			rec, ok = n.approvalCards[goalID]
			n.mu.Unlock()
			if ok {
				break
			}
		}
	}
	if !ok || rec.messageID == "" {
		// The card was never sent (restart window) — send it fresh.
		n.asyncSendCard(card)
		return
	}
	if !rec.hadPending {
		return // the card carried no hint — nothing to replace (the record stays for the resolved patch)
	}
	n.updateCard(rec.messageID, card)
}

// onGoalReviewResolved patches the card to its processed state when the
// human decided on the WEB (the button path patches through the Connector).
// The record survives the ready patch for exactly this purpose.
func (n *Notifier) onGoalReviewResolved(_ context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	decision, _ := m["decision"].(string)
	if goalID == "" || (decision != "approve" && decision != "reject" && decision != "redirect") {
		return
	}
	n.mu.Lock()
	rec, ok := n.approvalCards[goalID]
	if ok {
		delete(n.approvalCards, goalID)
	}
	n.mu.Unlock()
	if !ok || rec.messageID == "" {
		return // never sent / already handled — nothing to patch
	}
	card, err := buildProcessedCard(goalID, decision, rec.domainType == "scratch")
	if err != nil {
		return
	}
	n.updateCard(rec.messageID, card)
}

// reviewReadyPatchWait bounds how long the ready handler waits for the card
// send to record its message_id (the two handlers race on separate bus
// goroutines). Vars so tests shrink them.
var (
	reviewReadyPatchWait = 5 * time.Second
	reviewReadyPatchPoll = 20 * time.Millisecond
)

// sendApprovalCard sends the approval card and records its message_id for
// the review_ready patch.
func (n *Notifier) sendApprovalCard(goalID, cardJSON string, hadPending bool, domainType string) {
	go func() {
		msgID, err := n.SendCard(cardJSON)
		if err != nil {
			logging.Errorf("notify: feishu send card: %v", err)
			return
		}
		if msgID != "" {
			n.mu.Lock()
			n.approvalCards[goalID] = approvalCardRec{messageID: msgID, hadPending: hadPending, domainType: domainType}
			n.mu.Unlock()
		}
	}()
}

// updateCard PATCHes one sent card's content (the message-update API takes
// ONLY content — msg_type is immutable). Used by the review_ready opinion
// patch; the connector's processed-card replacement also goes through here.
func (n *Notifier) updateCard(messageID, content string) {
	if n.updateCardFn != nil {
		if err := n.updateCardFn(messageID, content); err != nil {
			logging.Infof("notify: update approval card %s: %v", messageID, err)
		}
		return
	}
	if n.client == nil {
		return
	}
	resp, err := n.client.Im.Message.Update(context.Background(),
		larkim.NewUpdateMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewUpdateMessageReqBodyBuilder().
				Content(content).
				Build()).
			Build())
	if err != nil {
		logging.Infof("notify: update approval card %s: %v", messageID, err)
		return
	}
	if !resp.Success() {
		logging.Infof("notify: update approval card %s failed: code=%d msg=%s", messageID, resp.Code, resp.Msg)
	}
}

// NOTE: the published event carries the PUBLISHER's ctx (often an HTTP
// request, cancelled the moment the handler returns) — DB work here would
// fail with "context canceled". Notify is background push; it uses its own
// ctx, not the event's.
func (n *Notifier) onGoalDelivered(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	note, _ := m["note"].(string)
	commits, _ := m["commits"].([]string)
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	body := fmt.Sprintf("**%s**  \n`goal %s`", title, short(goalID))
	if s := strings.TrimSpace(note); s != "" {
		body += "  \n" + s
	}
	if len(commits) > 0 {
		body += "  \n\n提交："
		for i, c := range commits {
			if i >= 5 {
				break
			}
			body += "  \n- `" + truncate(c, 90) + "`"
		}
	}
	// A scratch delivery has nothing merged — the note carries the semantics.
	if strings.Contains(note, "无仓库交付") {
		n.sendMilestoneCard("✅", "green", "任务完成", body)
		return
	}
	n.sendMilestoneCard("✅", "blue", "已自动合入", body)
}

// NOTE: the published event carries the PUBLISHER's ctx (often an HTTP
// request, cancelled the moment the handler returns) — DB work here would
// fail with "context canceled". Notify is background push; it uses its own
// ctx, not the event's.
func (n *Notifier) onGoalDeliverFailed(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	note, _ := m["note"].(string)
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	n.sendMilestoneCard("⚠️", "red", "合入失败", fmt.Sprintf("**%s**  \n`goal %s`  \n%s", title, short(goalID), truncate(note, 300)))
}

// NOTE: the published event carries the PUBLISHER's ctx (often an HTTP
// request, cancelled the moment the handler returns) — DB work here would
// fail with "context canceled". Notify is background push; it uses its own
// ctx, not the event's.
func (n *Notifier) onGoalFinished(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	status, _ := m["status"].(string)
	summary, _ := m["summary"].(string)
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	switch status {
	case "completed", "done":
		// The agent's full report (markdown) travels with the completion —
		// the card renders it (lark_md), so Feishu shows what the web
		// comment feed shows, not a bare title.
		body := fmt.Sprintf("**%s**  \n`goal %s`", title, short(goalID))
		if s := strings.TrimSpace(summary); s != "" {
			body += "  \n\n" + truncate(s, 2000)
		}
		n.sendMilestoneCard("🏁", "green", "完成", body)
	case "failed":
		n.sendMilestoneCard("❌", "red", "失败", fmt.Sprintf("**%s**  \n`goal %s`  \n%s", title, short(goalID), truncate(summary, 200)))
	}
}

// NOTE: the published event carries the PUBLISHER's ctx (often an HTTP
// request, cancelled the moment the handler returns) — DB work here would
// fail with "context canceled". Notify is background push; it uses its own
// ctx, not the event's.
func (n *Notifier) onRunCancelled(_ context.Context, e events.Event) {
	ctx := context.Background()
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	reason, _ := m["reason"].(string)
	// Platform/human-initiated cuts are NOT stalls: a handoff cut (the goal
	// changed hands — the old owner's run is terminated by design) and a
	// human stop (StopRun / Cancel, 决策 4-12) are normal control flow, not
	// "the task stalled". Only timeouts reach the human as 任务中断.
	// Structured reason_code — no string matching.
	if code, _ := m["reason_code"].(string); code == "handoff" || code == "stopped" {
		return
	}
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	n.sendMilestoneCard("⏱", "red", "任务中断", fmt.Sprintf("**%s**  \n`goal %s`  \n%s  \n\ngoal 保持 active，需人工处理", title, short(goalID), truncate(reason, 200)))
}

// goalTitle resolves a goal's title for milestone cards (best-effort; ” when
// the store is missing or the goal vanished).
func (n *Notifier) goalTitle(ctx context.Context, goalID string) string {
	if n.qs == nil {
		return ""
	}
	t, err := n.qs.GoalTitle(ctx, goalID)
	if err != nil {
		return ""
	}
	return t
}

// sendMilestoneCard builds and pushes a milestone card; falls back to the
// text form when the card cannot be built (the text path is the baseline).
func (n *Notifier) sendMilestoneCard(emoji, template, title, body string) {
	card, err := buildMilestoneCard(emoji, template, title, body)
	if err != nil {
		n.asyncSend(fmt.Sprintf("%s %s：%s", emoji, title, truncate(body, 300)))
		return
	}
	n.asyncSendCard(card)
}

// asyncSend pushes one message without blocking the bus handler.
func (n *Notifier) asyncSend(text string) {
	go func() {
		if err := n.Send(text); err != nil {
			logging.Errorf("notify: feishu send: %v", err)
		}
	}()
}

// asyncSendCard pushes one interactive card without blocking the bus handler.
func (n *Notifier) asyncSendCard(cardJSON string) {
	go func() {
		if _, err := n.SendCard(cardJSON); err != nil {
			logging.Errorf("notify: feishu send card: %v", err)
		}
	}()
}

// Send delivers a text message to the configured receive target via the
// Feishu IM API (the same channel the long connection authenticates).
func (n *Notifier) Send(text string) error {
	if n.send != nil {
		return n.send(text)
	}
	_, err := n.createMessage("text", fmt.Sprintf(`{"text":"%s"}`, escapeJSON(text)))
	return err
}

// SendCard delivers an interactive card (JSON 2.0, M3). The card content is
// a passthrough — msg_type=interactive with the card JSON as content.
// Returns the sent message's id (” for the injected test send) — the
// approval card records it for the review_ready patch (Option B).
func (n *Notifier) SendCard(cardJSON string) (string, error) {
	if n.send != nil {
		return "", n.send(cardJSON)
	}
	return n.createMessage("interactive", cardJSON)
}

func (n *Notifier) createMessage(msgType, content string) (string, error) {
	if n.receiveID == "" {
		return "", fmt.Errorf("notify: no receive target configured")
	}
	token, err := n.tenantToken()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{
		"receive_id": n.receiveID,
		"msg_type":   msgType,
		"content":    content,
	})
	req, err := http.NewRequest(http.MethodPost,
		"https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type="+n.receiveIDType,
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("feishu send: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("feishu send: parse response: %w", err)
	}
	if out.Code != 0 {
		// A stale token is worth ONE refresh-retry (the token lives 2h and the
		// daemon runs for days — a refresh race must not fail the message).
		if out.Code == 99991663 {
			n.mu.Lock()
			n.token = ""
			n.tokenExp = time.Time{}
			n.mu.Unlock()
			if token2, err := n.tenantToken(); err == nil {
				req.Header.Set("Authorization", "Bearer "+token2)
				resp2, err := http.DefaultClient.Do(req)
				if err == nil {
					defer resp2.Body.Close()
					var out2 struct {
						Code int    `json:"code"`
						Msg  string `json:"msg"`
						Data struct {
							MessageID string `json:"message_id"`
						} `json:"data"`
					}
					if err := json.NewDecoder(resp2.Body).Decode(&out2); err == nil && out2.Code == 0 {
						return out2.Data.MessageID, nil
					}
				}
			}
		}
		return "", fmt.Errorf("feishu send failed: code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Data.MessageID, nil
}

// tenantToken returns a valid tenant_access_token, fetching or refreshing it
// from app_id/app_secret when missing or near expiry. The token lives ~2h;
// the SDK's own token manager proved unreliable past expiry (99991663 on the
// daily digest after a night uptime), so the notifier owns it explicitly.
func (n *Notifier) tenantToken() (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.token != "" && time.Now().Before(n.tokenExp.Add(-5*time.Minute)) {
		return n.token, nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     n.appID,
		"app_secret": n.appSecret,
	})
	resp, err := http.Post("https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal",
		"application/json; charset=utf-8", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("feishu token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("feishu token: parse: %w", err)
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu token: code=%d msg=%s", out.Code, out.Msg)
	}
	n.token = out.TenantAccessToken
	n.tokenExp = time.Now().Add(time.Duration(out.Expire) * time.Second)
	return n.token, nil
}

func escapeJSON(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(s)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
