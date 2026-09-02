package notify

import (
	"context"
	"strings"
	"sync/atomic"
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
func (f *fakeQS) GoalTitle(ctx context.Context, goalID string) (string, error)  { return "g", nil }
func (f *fakeQS) AgentName(ctx context.Context, agentID string) (string, error) { return "PM", nil }
func (f *fakeQS) GoalDomainType(ctx context.Context, goalID string) (string, error) {
	return "repo", nil
}
func (f *fakeQS) GoalStatus(ctx context.Context, idPrefix string) (*GoalStatusView, error) {
	return nil, nil
}
func (f *fakeQS) Agents(ctx context.Context) ([]NamedID, error)   { return nil, nil }
func (f *fakeQS) Domains(ctx context.Context) ([]NamedID, error)  { return nil, nil }
func (f *fakeQS) Runtimes(ctx context.Context) ([]NamedID, error) { return nil, nil }
func (f *fakeQS) Skills(ctx context.Context) ([]NamedID, error)   { return nil, nil }
func (f *fakeQS) Squads(ctx context.Context) ([]NamedID, error)   { return nil, nil }
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

// TestReviewResolvedPatchesCard (Option B follow-up): a decision made on
// the WEB never touches the card's buttons — the review_resolved event
// patches the recorded card to its processed state.
func TestReviewResolvedPatchesCard(t *testing.T) {
	var patchedMsgID, patchedContent string
	n := New("app", "secret", "chat_id", "oc_mock")
	n.updateCardFn = func(messageID, content string) error {
		patchedMsgID, patchedContent = messageID, content
		return nil
	}
	n.mu.Lock()
	n.approvalCards["abc123456789"] = approvalCardRec{messageID: "om_mock_message", hadPending: true}
	n.mu.Unlock()
	bus := events.NewBus()
	n.Subscribe(bus)
	bus.Publish(context.Background(), events.Event{
		Topic: "goal:review_resolved", Payload: map[string]any{"goal_id": "abc123456789", "decision": "approve"},
	})
	deadline := time.Now().Add(2 * time.Second)
	for patchedMsgID == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if patchedMsgID != "om_mock_message" {
		t.Fatalf("the card must be patched to processed, got %q", patchedMsgID)
	}
	if !strings.Contains(patchedContent, "已批准") {
		t.Fatalf("the processed card must stamp the outcome, got: %s", patchedContent)
	}
}

// TestUnsubscribeStopsBusDelivery guards the orphan-Notifier bug: after
// Unsubscribe, a Notifier must no longer react to bus events. The
// disconnect-then-reconnect duplicate-card bug was caused by the old Notifier
// staying on the bus — every milestone event triggered N sends (one per
// Notifier ever created), so bot1 kept receiving cards after the owner
// switched to bot2.
func TestUnsubscribeStopsBusDelivery(t *testing.T) {
	sent := make(chan string, 4)
	n := New("app", "secret", "chat_id", "oc_mock")
	n.send = func(text string) error { sent <- text; return nil }
	bus := events.NewBus()
	n.Subscribe(bus)

	n.Unsubscribe()

	bus.Publish(context.Background(), events.Event{
		Topic: "goal:delivered", Payload: map[string]any{"goal_id": "abc123456789"},
	})
	select {
	case <-sent:
		t.Fatal("unsubscribed notifier must not receive events")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReconnectDoesNotDuplicateSends guards the full reconnect scenario:
// connect bot1 → disconnect → connect bot2 → publish one event → only ONE
// send (to bot2). Before the fix, bot1's orphan Notifier was still on the
// bus, so the event fanned out to both — bot1 got the card it should never
// have received.
func TestReconnectDoesNotDuplicateSends(t *testing.T) {
	bus := events.NewBus()

	// bot1: connect + subscribe
	bot1 := New("app1", "secret1", "chat_id", "oc_bot1")
	var bot1Sends atomic.Int32
	bot1.send = func(string) error { bot1Sends.Add(1); return nil }
	bot1.Subscribe(bus)

	// disconnect bot1 (Unsubscribe detaches it from the bus)
	bot1.Unsubscribe()

	// bot2: connect + subscribe
	bot2 := New("app2", "secret2", "chat_id", "oc_bot2")
	var bot2Sends atomic.Int32
	bot2.send = func(string) error { bot2Sends.Add(1); return nil }
	bot2.Subscribe(bus)

	bus.Publish(context.Background(), events.Event{
		Topic: "goal:delivered", Payload: map[string]any{"goal_id": "abc123456789"},
	})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if bot2Sends.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bot2Sends.Load() != 1 {
		t.Fatalf("bot2 must receive exactly 1 send, got %d", bot2Sends.Load())
	}
	if bot1Sends.Load() != 0 {
		t.Fatalf("bot1 (disconnected) must not receive any send, got %d", bot1Sends.Load())
	}
}
