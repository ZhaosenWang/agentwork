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

// fakeQS is a QueryStore stub for the Option B card tests.
type fakeQS struct {
	goals            []ReviewGoal
	pendingReviewers []string
}

func (f *fakeQS) ReviewGoals(ctx context.Context) ([]ReviewGoal, error) { return f.goals, nil }
func (f *fakeQS) PendingReviewers(ctx context.Context, goalID string) ([]string, error) {
	return f.pendingReviewers, nil
}
func (f *fakeQS) GoalTitle(ctx context.Context, goalID string) (string, error) { return "g", nil }
func (f *fakeQS) GoalStatus(ctx context.Context, idPrefix string) (*GoalStatusView, error) {
	return nil, nil
}
func (f *fakeQS) Agents(ctx context.Context) ([]NamedID, error)  { return nil, nil }
func (f *fakeQS) Domains(ctx context.Context) ([]NamedID, error) { return nil, nil }
func (f *fakeQS) TerminalSince(ctx context.Context, since, until string) ([]GoalBrief, error) {
	return nil, nil
}

// TestReviewCardCarriesPendingHint (Option B): while reviewers are working,
// the card names them and suggests the human MAY wait.
func TestReviewCardCarriesPendingHint(t *testing.T) {
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	n.SetQueryStore(&fakeQS{
		goals:            []ReviewGoal{{GoalID: "abc123456789", Title: "g", Reason: "merge"}},
		pendingReviewers: []string{"opencode", "claude"},
	})
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "goal:reviewing", Payload: map[string]any{"goal_id": "abc123456789", "reason": "merge"},
	})
	select {
	case card := <-sent:
		if !strings.Contains(card, "审查中") || !strings.Contains(card, "opencode") || !strings.Contains(card, "claude") {
			t.Fatalf("the card must name the reviewing agents, got: %s", card)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no card sent")
	}
}

// TestReviewReadyPatchesRecordedCard (Option B): goal:review_ready rebuilds
// the card with the opinions and PATCHES the SAME message (the hint is gone).
func TestReviewReadyPatchesRecordedCard(t *testing.T) {
	var patchedMsgID, patchedContent string
	n := New("app", "secret", "chat_id", "oc_mock")
	n.updateCardFn = func(messageID, content string) error {
		patchedMsgID, patchedContent = messageID, content
		return nil
	}
	n.SetQueryStore(&fakeQS{
		goals: []ReviewGoal{{GoalID: "abc123456789", Title: "g", Reason: "merge",
			Comments: []string{"意见：实现符合目标，可以直接通过"}}},
	})
	n.mu.Lock()
	n.approvalCards["abc123456789"] = approvalCardRec{messageID: "om_mock_message", hadPending: true}
	n.mu.Unlock()
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "goal:review_ready", Payload: map[string]any{"goal_id": "abc123456789"},
	})
	deadline := time.Now().Add(2 * time.Second)
	for patchedMsgID == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if patchedMsgID != "om_mock_message" {
		t.Fatalf("the SAME card must be patched, got %q", patchedMsgID)
	}
	if !strings.Contains(patchedContent, "审查意见") || !strings.Contains(patchedContent, "实现符合目标") {
		t.Fatalf("the patch must carry the opinions, got: %s", patchedContent)
	}
	if strings.Contains(patchedContent, "审查中") {
		t.Fatalf("the hint must be gone once opinions are in: %s", patchedContent)
	}
}

// TestReviewReadySendsFreshWhenCardMissing: a daemon restart between park
// and ready loses the in-memory message id — the ready event then sends a
// fresh card instead of patching nothing.
func TestReviewReadySendsFreshWhenCardMissing(t *testing.T) {
	old := reviewReadyPatchWait
	reviewReadyPatchWait = 50 * time.Millisecond
	defer func() { reviewReadyPatchWait = old }()
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	n.SetQueryStore(&fakeQS{
		goals: []ReviewGoal{{GoalID: "abc123456789", Title: "g", Reason: "merge"}},
	})
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "goal:review_ready", Payload: map[string]any{"goal_id": "abc123456789"},
	})
	select {
	case card := <-sent:
		if !strings.Contains(card, "待审批") {
			t.Fatalf("a fresh approval card must be sent, got: %s", card)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no card sent")
	}
}
