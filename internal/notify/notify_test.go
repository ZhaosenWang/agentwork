package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// TestReviewingEventText: a goal:reviewing event produces the approval
// notification (the M1 core: the human is told the checkpoint is waiting).
func TestReviewingEventText(t *testing.T) {
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "goal:reviewing",
		Payload: map[string]any{
			"goal_id": "abc123456789",
			"reason":  "merge: 合并前人工审批",
		},
	})

	select {
	case msg := <-sent:
		if !strings.Contains(msg, "待审批") || !strings.Contains(msg, "abc12345") {
			t.Fatalf("unexpected review notification: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification sent")
	}
}

// TestMilestoneEventTexts covers the milestone → text mapping for the
// delivered / deliver-failed / run-cancelled paths.
func TestMilestoneEventTexts(t *testing.T) {
	cases := []struct {
		topic   string
		payload map[string]any
		want    string
	}{
		{"goal:delivered", map[string]any{"goal_id": "abc123456789"}, "已自动合入"},
		{"goal:deliver_failed", map[string]any{"goal_id": "abc123456789", "note": "合并冲突"}, "合入失败"},
		{"run:cancelled", map[string]any{"run_id": "r123456789", "goal_id": "g123456789", "reason": "idle watchdog"}, "任务中断"},
	}
	for _, tc := range cases {
		t.Run(tc.topic, func(t *testing.T) {
			sent := make(chan string, 4)
			n := New("app", "secret", "chat_id", "oc_mock")
			n.send = func(text string) error { sent <- text; return nil }
			bus := events.NewBus()
			n.Subscribe(bus)
			bus.Publish(context.Background(), events.Event{Topic: tc.topic, Payload: tc.payload})
			select {
			case msg := <-sent:
				if !strings.Contains(msg, tc.want) {
					t.Fatalf("expected %q in %q", tc.want, msg)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("no notification for %s", tc.topic)
			}
		})
	}
}

// TestEscapeJSON guards the text-embedding path (a message with quotes/newlines
// must not corrupt the JSON content payload).
func TestEscapeJSON(t *testing.T) {
	out := escapeJSON(`say "hi"` + "\nnext")
	if out != `say \"hi\"\nnext` {
		t.Fatalf("unexpected escape: %q", out)
	}
}
