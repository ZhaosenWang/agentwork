// Package notify is the IM notification gateway (DESIGN.v2.md §11 M1,
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
	"fmt"
	"log"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/eushing/agentwork/internal/events"
)

// Notifier fans milestone events out to the Feishu long connection. The long
// connection itself is managed by the Connector (its SDK client is shared
// here for outbound sends); Notifier only maps events → messages and sends.
type Notifier struct {
	appID, appSecret string
	client           *lark.Client
	receiveIDType    string // chat_id | open_id
	receiveID        string // the owner's chat / open id
	qs               QueryStore // M3: read-only store for card evidence/digest; may be nil
	// send overrides the SDK delivery path (tests inject a mock; nil = the
	// lark client path). Kept unexported on purpose.
	send func(text string) error
}

// New creates a Notifier for a Feishu enterprise app (appID/appSecret).
// receiveIDType is "chat_id" or "open_id"; receiveID is where milestone
// pushes go (the owner's chat with the bot, or a group).
func New(appID, appSecret, receiveIDType, receiveID string) *Notifier {
	if receiveIDType == "" {
		receiveIDType = "chat_id"
	}
	return &Notifier{appID: appID, appSecret: appSecret, receiveIDType: receiveIDType, receiveID: receiveID}
}

// SetQueryStore wires the read-only store (M3): approval cards need the run
// evidence, milestone cards the goal title. Safe to leave nil — the notifier
// falls back to the event payload text.
func (n *Notifier) SetQueryStore(qs QueryStore) { n.qs = qs }

// Subscribe wires the milestone topics. Handlers run in the bus's own
// goroutines (fire-and-forget with panic recovery); each send runs in its
// own goroutine so a slow Feishu API call never blocks the bus.
func (n *Notifier) Subscribe(bus *events.Bus) {
	bus.Subscribe("goal:reviewing", n.onGoalReviewing)
	bus.Subscribe("goal:delivered", n.onGoalDelivered)
	bus.Subscribe("goal:deliver_failed", n.onGoalDeliverFailed)
	bus.Subscribe("goal:finished", n.onGoalFinished)
	bus.Subscribe("run:cancelled", n.onRunCancelled)
}

func (n *Notifier) onGoalReviewing(ctx context.Context, e events.Event) {
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
					if card, err := buildReviewCard(g); err == nil {
						n.asyncSendCard(card)
						return
					}
				}
			}
		}
	}
	n.asyncSend(fmt.Sprintf("🔔 待审批：goal %s 等你决定\n%s\n（批准后平台自动合入）", short(goalID), reason))
}

func (n *Notifier) onGoalDelivered(ctx context.Context, e events.Event) {
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
	n.sendMilestoneCard("✅", "blue", "已自动合入", body)
}

func (n *Notifier) onGoalDeliverFailed(ctx context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	note, _ := m["note"].(string)
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	n.sendMilestoneCard("⚠️", "red", "合入失败", fmt.Sprintf("**%s**  \n`goal %s`  \n%s", title, short(goalID), truncate(note, 300)))
}

func (n *Notifier) onGoalFinished(ctx context.Context, e events.Event) {
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

func (n *Notifier) onRunCancelled(ctx context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	reason, _ := m["reason"].(string)
	title := n.goalTitle(ctx, goalID)
	if title == "" {
		title = short(goalID)
	}
	n.sendMilestoneCard("⏱", "red", "任务中断", fmt.Sprintf("**%s**  \n`goal %s`  \n%s  \n\ngoal 保持 active，需人工处理", title, short(goalID), truncate(reason, 200)))
}

// goalTitle resolves a goal's title for milestone cards (best-effort; '' when
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
			log.Printf("notify: feishu send: %v", err)
		}
	}()
}

// asyncSendCard pushes one interactive card without blocking the bus handler.
func (n *Notifier) asyncSendCard(cardJSON string) {
	go func() {
		if err := n.SendCard(cardJSON); err != nil {
			log.Printf("notify: feishu send card: %v", err)
		}
	}()
}

// Send delivers a text message to the configured receive target via the
// Feishu IM API (the same channel the long connection authenticates).
func (n *Notifier) Send(text string) error {
	if n.send != nil {
		return n.send(text)
	}
	return n.createMessage("text", fmt.Sprintf(`{"text":"%s"}`, escapeJSON(text)))
}

// SendCard delivers an interactive card (JSON 2.0, M3). The card content is
// a passthrough — msg_type=interactive with the card JSON as content.
func (n *Notifier) SendCard(cardJSON string) error {
	if n.send != nil {
		return n.send(cardJSON)
	}
	return n.createMessage("interactive", cardJSON)
}

func (n *Notifier) createMessage(msgType, content string) error {
	if n.client == nil {
		return fmt.Errorf("notify: client not initialized (Start not called)")
	}
	if n.receiveID == "" {
		return fmt.Errorf("notify: no receive target configured")
	}
	resp, err := n.client.Im.Message.Create(context.Background(),
		larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(n.receiveIDType).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				MsgType(msgType).
				ReceiveId(n.receiveID).
				Content(content).
				Build()).
			Build())
	if err != nil {
		return fmt.Errorf("feishu send: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu send failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
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
