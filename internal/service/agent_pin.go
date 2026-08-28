package service

import (
	"context"
	"fmt"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// AgentPinService manages the sidebar-pin preference for agents. Pin state is
// per-agent only — agentwork is a single-user local platform (no user/tenant
// dimension), so there is no user_id column.
//
// The model is "row existence = pinned": pin = INSERT OR IGNORE, unpin =
// DELETE, and SELECT agent_id FROM agent_pin is the pinned set. The PK on
// agent_id makes toggle idempotent (one pin per agent); no pinned boolean
// column or UPSERT is needed.
type AgentPinService struct {
	st  *store.Store
	bus *events.Bus
}

func NewAgentPinService(st *store.Store, bus *events.Bus) *AgentPinService {
	return &AgentPinService{st: st, bus: bus}
}

// SetPinned toggles an agent's sidebar-pin state. Idempotent: pinning an
// already-pinned agent and unpinning a non-pinned one are both no-ops.
//
// Pinning requires the agent to exist (mustExist → validation error / HTTP 400
// otherwise, matching AgentService.Update's existence guard). Unpinning does
// NOT check existence: a deleted agent's pin row is already cleaned up by
// AgentService.Delete, and an unknown id simply matches zero rows — either way
// the DELETE is a no-op, which is the idempotent end state the caller wants.
func (s *AgentPinService) SetPinned(ctx context.Context, agentID string, pinned bool) error {
	if pinned {
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, agentID, "agent"); err != nil {
			return err
		}
		// INSERT OR IGNORE: a second pin on the same agent is a no-op rather
		// than a UNIQUE-violation — the end state is what the caller asked for.
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT OR IGNORE INTO agent_pin (agent_id, created_at) VALUES (?, ?)`,
			agentID, now()); err != nil {
			return fmt.Errorf("pin agent: %w", err)
		}
	} else {
		if _, err := s.st.DB().ExecContext(ctx,
			`DELETE FROM agent_pin WHERE agent_id=?`, agentID); err != nil {
			return fmt.Errorf("unpin agent: %w", err)
		}
	}
	// Fire-and-forget: a ws hub subscriber can push pin changes to the frontend
	// in real time; with no subscriber (tests, daemon not up) this is a no-op.
	s.bus.Publish(ctx, events.Event{
		Topic:   "agent:pin_changed",
		Payload: map[string]any{"agent_id": agentID, "pinned": pinned},
	})
	return nil
}

// ListPinned returns the IDs of agents currently pinned to the sidebar,
// ordered by pin time (oldest first — the sidebar renders in this order).
// Returns the IDs only: the caller already has the full agent objects from
// GET /agents and just needs the set to mark button state.
func (s *AgentPinService) ListPinned(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT agent_id FROM agent_pin ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list pinned agents: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
