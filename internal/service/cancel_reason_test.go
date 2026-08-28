package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// TestCancelledRunPostsSystemFeedComment: a cancelled run with a structured
// cancel_reason posts a SYSTEM feed comment (author_type='system') describing
// the cancellation cause — the goal feed must show WHY the run was cut, not
// just "cancelled by platform" (platform noise, not the agent's words).
func TestCancelledRunPostsSystemFeedComment(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatal(err)
	}
	r := enqueueFirst(t, rs, g)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=?, cancel_reason='idle_watchdog' WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, r.ID, "cancelled", "cancelled by platform"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	comments, _ := cs.List(ctx, g.ID)
	var found bool
	for _, c := range comments {
		if c.AuthorType == "system" && c.RunID == r.ID && strings.Contains(c.Content, "idle_watchdog") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancelled run must post a system feed comment with the cancel_reason, got %+v", comments)
	}
}

// TestCancelledRunNoReasonNoComment: a cancelled run with NO cancel_reason
// (empty string) posts NO system comment — the guard prevents noise when the
// structured reason is unavailable (legacy paths, test fixtures).
func TestCancelledRunNoReasonNoComment(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatal(err)
	}
	r := enqueueFirst(t, rs, g)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, r.ID, "cancelled", "cancelled by platform"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	comments, _ := cs.List(ctx, g.ID)
	for _, c := range comments {
		if c.AuthorType == "system" && c.RunID == r.ID {
			t.Fatalf("cancelled run with no cancel_reason must NOT post a system comment, got %+v", comments)
		}
	}
}

// TestFinishCancelledPublishesRunCancelledEvent: Finish (the terminal
// chokepoint) publishes run:cancelled with the structured reason_code when a
// cancelled run carries a cancel_reason. This covers the machine-watchdog
// path (idle_watchdog / timeout) which previously had NO event and left the
// notify layer blind (no Feishu "任务中断" card).
func TestFinishCancelledPublishesRunCancelledEvent(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatal(err)
	}
	r := enqueueFirst(t, rs, g)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=?, cancel_reason='timeout' WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), r.ID); err != nil {
		t.Fatal(err)
	}
	var gotReasonCode string
	done := make(chan struct{})
	gs.bus.Subscribe("run:cancelled", func(_ context.Context, e events.Event) {
		m, _ := e.Payload.(map[string]any)
		gotReasonCode, _ = m["reason_code"].(string)
		close(done)
	})
	if err := rs.Finish(ctx, r.ID, "cancelled", "cancelled by platform"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run:cancelled event was not published within 2s")
	}
	if gotReasonCode != "timeout" {
		t.Fatalf("reason_code, got %q want %q", gotReasonCode, "timeout")
	}
}
