package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Agent is a runtime + a persona. Under the per-task connection model, creating
// an agent does NOT launch a process: a fresh connection is opened per run.
// status/pid columns are deliberately gone — they belong to the future
// long-lived-session model and would be dead columns today.
type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	// Description is a human-facing one-liner shown in the web list — what
	// this agent does in a glance. Distinct from SystemPrompt (the persona
	// shipped to the model): description is for the operator browsing agents,
	// not the agent itself.
	Description  string            `json:"description"`
	RuntimeID    string            `json:"runtime_id"`
	SystemPrompt string            `json:"system_prompt"`
	Model        string            `json:"model"`
	Env          map[string]string `json:"env"`
	// McpServers are EXTRA MCP servers advertised at session/new alongside
	// the platform's workspace server — the agent's own tools (browser,
	// database, an external ACP agent via an MCP bridge, ...). Type speaks
	// acp.McpServer (type stdio|http|sse, name, url or command/args).
	McpServers    []acp.McpServer `json:"mcp_servers"`
	// Skills are the platform-managed skill ids selected for this agent
	// (CLI 分支 Phase 4) — pushed to the agent's machine via config.push.
	Skills        []string        `json:"skills"`
	MaxConcurrent int             `json:"max_concurrent"`
	CreatedAt     string          `json:"created_at"`
}

type AgentService struct {
	st  *store.Store
	bus *events.Bus
}

func NewAgentService(st *store.Store, bus *events.Bus) *AgentService {
	return &AgentService{st: st, bus: bus}
}

func (s *AgentService) Create(ctx context.Context, a Agent) (*Agent, error) {
	if a.Name == "" {
		return nil, NewFieldRequiredError("name")
	}
	if a.RuntimeID == "" {
		return nil, NewFieldRequiredError("runtime_id")
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
	if a.McpServers == nil {
		a.McpServers = []acp.McpServer{}
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	a.ID = newID()
	a.CreatedAt = now()
	envJSON, _ := json.Marshal(a.Env)
	mcpJSON, _ := json.Marshal(a.McpServers)
	skillsJSON, _ := json.Marshal(a.Skills)
	_, err = s.st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Description, a.RuntimeID, a.SystemPrompt, a.Model, string(envJSON), string(mcpJSON), skillsJSON, a.MaxConcurrent, a.CreatedAt)
	if err != nil {
		if ve := dupNameCodedError(err, CodeAgentNameExists, "agent", a.Name); ve != nil {
			return nil, ve
		}
		return nil, fmt.Errorf("insert agent: %w", err)
	}
	// Notify the daemon that an agent exists (it rebuilds per-agent workers on
	// startup; this is the live-create path). Under the per-task model no
	// process is launched here.
	s.bus.Publish(ctx, events.Event{Topic: "agent:created", Payload: a})
	return &a, nil
}

// Update edits an agent's mutable fields: name, description, runtime_id,
// system_prompt, model, max_concurrent, env, mcp_servers, skills. A changed
// system_prompt/runtime_id/model takes effect on the agent's NEXT run —
// in-flight runs keep their snapshot.
func (s *AgentService) Update(ctx context.Context, id string, a Agent) (*Agent, error) {
	if a.Name == "" {
		return nil, NewFieldRequiredError("name")
	}
	if a.RuntimeID == "" {
		return nil, NewFieldRequiredError("runtime_id")
	}
	// Verify the target runtime exists (FK guard) — same as Create.
	var rtID string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM runtime WHERE id=?`, a.RuntimeID).Scan(&rtID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("check runtime: %w", err)
	}
	if a.MaxConcurrent < 1 {
		a.MaxConcurrent = 1
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, id, "agent"); err != nil {
		return nil, err
	}
	envJSON, _ := json.Marshal(a.Env)
	mcpJSON, _ := json.Marshal(a.McpServers)
	skillsJSON, _ := json.Marshal(a.Skills)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE agent SET name=?, description=?, runtime_id=?, system_prompt=?, model=?, max_concurrent=?, env=?, mcp_servers=?, skills=? WHERE id=?`,
		a.Name, a.Description, a.RuntimeID, a.SystemPrompt, a.Model, a.MaxConcurrent, string(envJSON), string(mcpJSON), string(skillsJSON), id); err != nil {
		if ve := dupNameCodedError(err, CodeAgentNameExists, "agent", a.Name); ve != nil {
			return nil, ve
		}
		return nil, fmt.Errorf("update agent: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *AgentService) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at
		 FROM agent ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		var envJSON, mcpJSON, skillsJSON string
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(envJSON), &a.Env)
		_ = json.Unmarshal([]byte(mcpJSON), &a.McpServers)
		_ = json.Unmarshal([]byte(skillsJSON), &a.Skills)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *AgentService) Get(ctx context.Context, id string) (*Agent, error) {
	var a Agent
	var envJSON, mcpJSON, skillsJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at
		 FROM agent WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(envJSON), &a.Env)
	_ = json.Unmarshal([]byte(mcpJSON), &a.McpServers)
	_ = json.Unmarshal([]byte(skillsJSON), &a.Skills)
	return &a, nil
}

func (s *AgentService) Delete(ctx context.Context, id string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Hard-reject guards: any referencing row blocks the delete and forces the
	// caller to clean up first. goal/schedule/squad/run all hold an agent id;
	// issue_assignee makes this agent an issue-handling target. Only when zero
	// references remain does the delete proceed (plan §守卫矩阵).
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE assignee_type='agent' AND assignee_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check goals: %w", err)
	}
	if n > 0 {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeAgentHasGoals,
			fmt.Sprintf("agent %q has %d goal(s); delete or reassign them first", name, n),
			map[string]any{"name": name, "count": n})
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schedule WHERE assignee_type='agent' AND assignee_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check schedules: %w", err)
	}
	if n > 0 {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeAgentHasSchedules,
			fmt.Sprintf("agent %q has %d schedule(s); delete or reassign them first", name, n),
			map[string]any{"name": name, "count": n})
	}
	// squad.leader_id is RESTRICT — a leaderless squad is invalid.
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad WHERE leader_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check leader role: %w", err)
	}
	if n > 0 {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeAgentLeadsSquad,
			fmt.Sprintf("agent %q leads %d squad(s); reassign leadership first", name, n),
			map[string]any{"name": name, "count": n})
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad_member WHERE member_type='agent' AND member_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check squad membership: %w", err)
	}
	if n > 0 {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeAgentInSquads,
			fmt.Sprintf("agent %q is a member of %d squad(s); remove them from squads first", name, n),
			map[string]any{"name": name, "count": n})
	}
	// Only a RUNNING run blocks — completed/failed/cancelled runs are history.
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE agent_id=? AND status='running'`, id).Scan(&n); err != nil {
		return fmt.Errorf("check running runs: %w", err)
	}
	if n > 0 {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeAgentHasRunningRuns,
			fmt.Sprintf("agent %q has %d running run(s); wait for them to finish or cancel them first", name, n),
			map[string]any{"name": name, "count": n})
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM domain WHERE issue_assignee=? AND issue_assignee_type='agent'`, id).Scan(&n); err != nil {
		return fmt.Errorf("check issue assignee: %w", err)
	}
	if n > 0 {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeAgentHandlesIssues,
			fmt.Sprintf("agent %q handles issues for %d domain(s); reassign issue handling first", name, n),
			map[string]any{"name": name, "count": n})
	}
	// All guards passed (zero referencing rows). Clean up the FK-constrained
	// history before the agent row can go: run.agent_id is a non-cascading FK,
	// so a run must be dropped (with its chat_message cache) before the agent.
	// Schedules/goals/squad_members pointing at this agent are already gone
	// (the guards above refused while any existed).
	for _, stmt := range []string{
		`DELETE FROM chat_message WHERE run_id IN (SELECT id FROM run WHERE agent_id=?)`,
		`DELETE FROM run WHERE agent_id=?`,
		// v2 (决策 6-1): sub_goal has no FK on assignee/verifier — a deleted
		// agent must not leave work items that later enqueue a run on a dead
		// agent id (the run's agent FK would reject it and the sub-goal would
		// stall running forever). Cancel the agent's non-terminal sub-goals;
		// terminal history (verified/failed/cancelled) stays, same as
		// CancelSubGoal.
		`UPDATE sub_goal SET status='cancelled' WHERE (assignee_id=?1 OR verifier_id=?1) AND status NOT IN ('verified','cancelled','failed')`,
		`DELETE FROM agent WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("delete agent dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Tell the daemon to drop this agent's worker.
	s.bus.Publish(ctx, events.Event{Topic: "agent:deleted", Payload: map[string]string{"id": id}})
	return nil
}
