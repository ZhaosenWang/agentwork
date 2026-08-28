package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// newPinSvc builds an AgentPinService on a fresh in-memory store. Each test
// is independent (its own DB + bus).
func newPinSvc(t *testing.T) (*AgentPinService, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	return NewAgentPinService(st, events.NewBus()), st
}

func TestAgentPinSetAndList(t *testing.T) {
	ctx := context.Background()
	svc, st := newPinSvc(t)
	agentID := seedAgent(t, st, "test-agent")

	// Pin → list returns exactly this id.
	if err := svc.SetPinned(ctx, agentID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pinned, err := svc.ListPinned(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pinned) != 1 || pinned[0] != agentID {
		t.Fatalf("expected [%s], got %v", agentID, pinned)
	}

	// Pin again (idempotent) → still one row, no duplicate, no error.
	if err := svc.SetPinned(ctx, agentID, true); err != nil {
		t.Fatalf("idempotent pin: %v", err)
	}
	pinned, _ = svc.ListPinned(ctx)
	if len(pinned) != 1 {
		t.Fatalf("duplicate pin produced %d rows, want 1: %v", len(pinned), pinned)
	}

	// Unpin → empty.
	if err := svc.SetPinned(ctx, agentID, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	pinned, _ = svc.ListPinned(ctx)
	if len(pinned) != 0 {
		t.Fatalf("expected empty after unpin, got %v", pinned)
	}

	// Unpin again (idempotent) → still empty, no error.
	if err := svc.SetPinned(ctx, agentID, false); err != nil {
		t.Fatalf("idempotent unpin: %v", err)
	}
	pinned, _ = svc.ListPinned(ctx)
	if len(pinned) != 0 {
		t.Fatalf("expected empty after idempotent unpin, got %v", pinned)
	}
}

func TestAgentPinNonexistentAgent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newPinSvc(t)

	// Pinning a nonexistent agent id → validation error (agent does not
	// exist). The guard mirrors AgentService.Update.
	err := svc.SetPinned(ctx, "no-such-agent", true)
	if err == nil {
		t.Fatal("expected error pinning nonexistent agent, got nil")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
	// Nothing was written.
	pinned, _ := svc.ListPinned(ctx)
	if len(pinned) != 0 {
		t.Fatalf("nonexistent pin leaked a row: %v", pinned)
	}

	// Unpinning a nonexistent agent → no error (DELETE with no match is a
	// no-op; existence is only checked on the pin path, matching the
	// "row existence = state" model).
	if err := svc.SetPinned(ctx, "no-such-agent", false); err != nil {
		t.Fatalf("unpinning nonexistent agent should be a no-op, got %v", err)
	}
}

func TestAgentPinDeleteAgentCleansUp(t *testing.T) {
	ctx := context.Background()
	svc, st := newPinSvc(t)
	agentID := seedAgent(t, st, "doomed-agent")

	if err := svc.SetPinned(ctx, agentID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// seedAgent creates a dependency-free agent (no goals/schedules/squads/
	// runs), so Delete's hard-reject guards all pass and the cleanup loop
	// runs — including the agent_pin row.
	agentSvc := NewAgentService(st, events.NewBus())
	if err := agentSvc.Delete(ctx, agentID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	pinned, _ := svc.ListPinned(ctx)
	if len(pinned) != 0 {
		t.Fatalf("pin row survived agent delete: %v", pinned)
	}
	// Belt-and-suspenders: the table itself has no rows for this agent.
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_pin WHERE agent_id=?`, agentID).Scan(&n); err != nil {
		t.Fatalf("count agent_pin: %v", err)
	}
	if n != 0 {
		t.Fatalf("agent_pin has %d leftover rows for deleted agent", n)
	}
}

func TestAgentPinOrdering(t *testing.T) {
	ctx := context.Background()
	svc, st := newPinSvc(t)
	// seedAgent uses unique runtime/agent names per call, so three distinct
	// agents in one store is fine.
	a1 := seedAgent(t, st, "alpha")
	a2 := seedAgent(t, st, "beta")
	a3 := seedAgent(t, st, "gamma")

	// Pin in order; created_at drives the sort (oldest first).
	for _, id := range []string{a1, a2, a3} {
		if err := svc.SetPinned(ctx, id, true); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
		time.Sleep(2 * time.Millisecond) // separate timestamps (RFC3339Nano)
	}
	pinned, _ := svc.ListPinned(ctx)
	if len(pinned) != 3 {
		t.Fatalf("expected 3 pinned, got %d: %v", len(pinned), pinned)
	}
	want := []string{a1, a2, a3}
	for i := range want {
		if pinned[i] != want[i] {
			t.Fatalf("order mismatch at %d: got %s want %s (full: %v)", i, pinned[i], want[i], pinned)
		}
	}
}

func TestAgentPinEventPublished(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	bus := events.NewBus()
	svc := NewAgentPinService(st, bus)
	agentID := seedAgent(t, st, "evented-agent")

	// Publish is async (each handler runs in its own goroutine), so receive
	// on a buffered channel and wait on it to synchronize.
	got := make(chan events.Event, 1)
	bus.Subscribe("agent:pin_changed", func(_ context.Context, e events.Event) {
		got <- e
	})

	if err := svc.SetPinned(ctx, agentID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	select {
	case e := <-got:
		if e.Topic != "agent:pin_changed" {
			t.Fatalf("topic: got %q want agent:pin_changed", e.Topic)
		}
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			t.Fatalf("payload type: %T", e.Payload)
		}
		if payload["agent_id"] != agentID {
			t.Fatalf("agent_id: got %v want %s", payload["agent_id"], agentID)
		}
		if pinned, _ := payload["pinned"].(bool); !pinned {
			t.Fatalf("pinned: got %v want true", payload["pinned"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent:pin_changed event")
	}
}
