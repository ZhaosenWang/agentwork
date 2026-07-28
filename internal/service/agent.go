package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Agent is a runtime + a persona. Creating one launches a long-lived ACP
// server (handled by the daemon via the agent:created event).
type Agent struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	RuntimeID     string            `json:"runtime_id"`
	SystemPrompt  string            `json:"system_prompt"`
	Model         string            `json:"model"`
	WorkdirBase   string            `json:"workdir_base"`
	Env           map[string]string `json:"env"`
	MaxConcurrent int               `json:"max_concurrent"`
	Status        string            `json:"status"`
	PID           int               `json:"pid"`
	CreatedAt     string            `json:"created_at"`
}

type AgentService struct {
	st  *store.Store
	bus *events.Bus
}

func NewAgentService(st *store.Store, bus *events.Bus) *AgentService {
	return &AgentService{st: st, bus: bus}
}

func (s *AgentService) Create(ctx context.Context, a Agent) (*Agent, error) {
	if a.Name == "" || a.RuntimeID == "" {
		return nil, NewValidationError("name and runtime_id are required")
	}
	// Verify runtime exists.
	var rtID string
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM runtime WHERE id=?`, a.RuntimeID).Scan(&rtID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.MaxConcurrent < 1 {
		a.MaxConcurrent = 1
	}
	a.ID = newID()
	a.Status = "offline" // daemon will flip to online once the server is up
	a.CreatedAt = now()
	envJSON, _ := json.Marshal(a.Env)
	_, err = s.st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,description,runtime_id,system_prompt,model,workdir_base,env,max_concurrent,status,pid,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Description, a.RuntimeID, a.SystemPrompt, a.Model, a.WorkdirBase, string(envJSON), a.MaxConcurrent, a.Status, 0, a.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert agent: %w", err)
	}
	// Tell the daemon to launch the ACP server for this agent.
	s.bus.Publish(ctx, events.Event{Topic: "agent:created", Payload: a})
	return &a, nil
}

func (s *AgentService) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,workdir_base,env,max_concurrent,status,pid,created_at
		 FROM agent ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var envJSON string
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &a.WorkdirBase, &envJSON, &a.MaxConcurrent, &a.Status, &a.PID, &a.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(envJSON), &a.Env)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *AgentService) Get(ctx context.Context, id string) (*Agent, error) {
	var a Agent
	var envJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,workdir_base,env,max_concurrent,status,pid,created_at
		 FROM agent WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &a.WorkdirBase, &envJSON, &a.MaxConcurrent, &a.Status, &a.PID, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(envJSON), &a.Env)
	return &a, nil
}

func (s *AgentService) Delete(ctx context.Context, id string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Clean dependents in FK order. task_queue/session/schedule reference
	// agent; task.assignee_id is NOT FK-constrained so we null it out to
	// avoid orphan pointers. Schedules pointing at this agent are dropped
	// (a schedule without its agent is meaningless); their run history goes
	// too. task_queue rows for this agent are dropped (they were dispatch
	// slots, not durable state — the task itself survives in backlog).
	for _, stmt := range []string{
		`DELETE FROM task_queue WHERE agent_id=?`,
		`DELETE FROM session WHERE agent_id=?`,
		`DELETE FROM schedule_run WHERE schedule_id IN (SELECT id FROM schedule WHERE assignee_id=?)`,
		`DELETE FROM schedule WHERE assignee_id=?`,
		`UPDATE task SET assignee_type='human', assignee_id='' WHERE assignee_type='agent' AND assignee_id=?`,
		`DELETE FROM agent WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("delete agent dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Tell the daemon to stop the subprocess.
	s.bus.Publish(ctx, events.Event{Topic: "agent:deleted", Payload: map[string]string{"id": id}})
	return nil
}
