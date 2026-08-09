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

func (n *Notifier) onGoalReviewing(_ context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	reason, _ := m["reason"].(string)
	n.asyncSend(fmt.Sprintf("🔔 待审批：goal %s 等你决定\n%s\n（批准后平台自动合入）", short(goalID), reason))
}

func (n *Notifier) onGoalDelivered(_ context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	n.asyncSend(fmt.Sprintf("✅ 已自动合入：goal %s", short(goalID)))
}

func (n *Notifier) onGoalDeliverFailed(_ context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	note, _ := m["note"].(string)
	n.asyncSend(fmt.Sprintf("⚠️ 合入失败：goal %s\n%s", short(goalID), truncate(note, 300)))
}

func (n *Notifier) onGoalFinished(_ context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	goalID, _ := m["goal_id"].(string)
	status, _ := m["status"].(string)
	summary, _ := m["summary"].(string)
	switch status {
	case "completed", "done":
		n.asyncSend(fmt.Sprintf("🏁 goal %s 完成", short(goalID)))
	case "failed":
		n.asyncSend(fmt.Sprintf("❌ goal %s 失败：%s", short(goalID), truncate(summary, 200)))
	}
}

func (n *Notifier) onRunCancelled(_ context.Context, e events.Event) {
	m, _ := e.Payload.(map[string]any)
	runID, _ := m["run_id"].(string)
	goalID, _ := m["goal_id"].(string)
	reason, _ := m["reason"].(string)
	n.asyncSend(fmt.Sprintf("⏱ 任务中断：run %s（goal %s）\n%s\ngoal 保持 active，需人工处理", short(runID), short(goalID), truncate(reason, 200)))
}

// asyncSend pushes one message without blocking the bus handler.
func (n *Notifier) asyncSend(text string) {
	go func() {
		if err := n.Send(text); err != nil {
			log.Printf("notify: feishu send: %v", err)
		}
	}()
}

// Send delivers a text message to the configured receive target via the
// Feishu IM API (the same channel the long connection authenticates).
func (n *Notifier) Send(text string) error {
	if n.send != nil {
		return n.send(text)
	}
	if n.client == nil {
		return fmt.Errorf("notify: client not initialized (Start not called)")
	}
	if n.receiveID == "" {
		return fmt.Errorf("notify: no receive target configured")
	}
	content := fmt.Sprintf(`{"text":"%s"}`, escapeJSON(text))
	resp, err := n.client.Im.Message.Create(context.Background(),
		larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(n.receiveIDType).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				MsgType("text").
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
