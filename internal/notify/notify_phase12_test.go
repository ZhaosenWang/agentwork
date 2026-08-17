package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// TestAgentQuestionEventSendsCard (决策 7-3): a comment:agent_question event
// (an agent's --ask question) pushes a Feishu ask card whose title names the
// questioning agent (❓ {agentName}), body carries the goal title + the
// question, and a reply form lets the human answer inline. The platform is
// single-user, so no per-goal recipient resolution — the card goes to the one
// receive target.
func TestAgentQuestionEventSendsCard(t *testing.T) {
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "comment:agent_question",
		Payload: map[string]any{
			"goal_id":    "abc123456789",
			"comment_id": "cmt-ask-1",
			"agent_id":   "agent-1",
			"question":   "这个接口的入参用 JSON 还是 form？",
		},
	})
	select {
	case card := <-sent:
		// Title names the questioning agent (no qs → falls back to id prefix).
		if !strings.Contains(card, `"content":"❓ agent-1"`) {
			t.Fatalf("the card title must be '❓ {agentName}', got: %s", card)
		}
		// Body carries the question in the "{agentName} 询问：" line.
		if !strings.Contains(card, "agent-1 询问：") || !strings.Contains(card, "JSON 还是 form") {
			t.Fatalf("the card body must carry the question, got: %s", card)
		}
		// The reply form is present with the ask comment id (the reply's parent).
		// The container tag is "form" (Feishu's im/v1 API rejects "form_container").
		if !strings.Contains(card, `"tag":"form"`) || !strings.Contains(card, `"comment_id":"cmt-ask-1"`) {
			t.Fatalf("the card must carry a reply form keyed to the ask comment, got: %s", card)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no card sent for comment:agent_question")
	}
}

// TestAgentQuestionEventNoEmptyQuestion: an event with an empty question (or
// empty goal_id) is dropped — never pushes a blank card. This guards the
// guard clause in onAgentQuestion.
func TestAgentQuestionEventNoEmptyQuestion(t *testing.T) {
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	bus := events.NewBus()
	n.Subscribe(bus)
	for _, tc := range []struct {
		name  string
		pload map[string]any
	}{
		{"empty question", map[string]any{"goal_id": "abc123456789", "question": "   "}},
		{"empty goal_id", map[string]any{"goal_id": "", "question": "hi"}},
		{"blank both", map[string]any{"goal_id": "", "question": ""}},
	} {
		bus.Publish(context.Background(), events.Event{Topic: "comment:agent_question", Payload: tc.pload})
	}
	// No card should arrive for any of the empty-payload cases. Give the
	// goroutines a beat to (not) fire.
	select {
	case card := <-sent:
		t.Fatalf("no card should be sent for empty question/goal_id, got: %s", card)
	case <-time.After(300 * time.Millisecond):
		// expected — no card fired.
	}
}

// TestAgentQuestionCardUsesGoalTitleAndAgentName: when a QueryStore is wired,
// the card's body carries the resolved goal title and the title names the
// resolved agent (not the id-prefix fallbacks).
func TestAgentQuestionCardUsesGoalTitleAndAgentName(t *testing.T) {
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	n.SetQueryStore(&fakeQS{}) // fakeQS.GoalTitle→"g", AgentName→"PM"
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "comment:agent_question",
		Payload: map[string]any{
			"goal_id":    "abc123456789",
			"comment_id": "cmt-ask-2",
			"agent_id":   "pm-agent-id",
			"question":   "确认一下方向",
		},
	})
	select {
	case card := <-sent:
		// Title is the resolved agent name (fakeQS.AgentName returns "PM").
		if !strings.Contains(card, `"content":"❓ PM"`) {
			t.Fatalf("ask card title must name the resolved agent, got: %s", card)
		}
		// Body carries the goal title (fakeQS.GoalTitle returns "g"). The
		// card is JSON, so newlines are escaped as \n — match the escaped form.
		if !strings.Contains(card, `**任务：**\ng`) {
			t.Fatalf("the card body must carry the resolved goal title, got: %s", card)
		}
		// Body carries the agent name in the "询问" line.
		if !strings.Contains(card, "PM 询问：") {
			t.Fatalf("the card body must name the agent in the question line, got: %s", card)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no card sent")
	}
}

// TestBuildAskCardStructure pins the card's JSON shape: header title names
// the agent, body has the goal + question markdown, and the form_container
// carries an input + a form_submit button whose value keys the reply to the
// ask comment (onCardAction reads action=reply_ask + comment_id).
func TestBuildAskCardStructure(t *testing.T) {
	card, err := buildAskCard("Coder", "加 string utils", "用 JSON 吗？", "goal-1", "cmt-ask-9")
	if err != nil {
		t.Fatalf("buildAskCard: %v", err)
	}
	for _, want := range []string{
		`"content":"❓ Coder"`,           // title
		`"template":"blue"`,             // blue header
		`**任务：**`,                       // body label
		`加 string utils`,                 // goal title
		`**Coder 询问：**`,                 // question label
		`用 JSON 吗？`,                     // question
		`"tag":"form"`,                   // reply form (NOT form_container — Feishu rejects it)
		`"name":"reply_text"`,            // input field
		`"action_type":"form_submit"`,    // submit button
		`"action":"reply_ask"`,           // reply action
		`"comment_id":"cmt-ask-9"`,       // ask comment id (reply parent)
		`"goal_id":"goal-1"`,             // goal id
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("ask card missing %q, got: %s", want, card)
		}
	}
}

// TestBuildAskCardFallsBackWhenNoAgentName: an empty agentName renders the
// generic "Agent" title (not an empty ❓), so a deleted-agent edge case still
// produces a readable card.
func TestBuildAskCardFallsBackWhenNoAgentName(t *testing.T) {
	card, _ := buildAskCard("", "g", "q?", "goal-1", "cmt-1")
	if !strings.Contains(card, `"content":"❓ Agent"`) {
		t.Fatalf("empty agentName must fall back to 'Agent', got: %s", card)
	}
}
