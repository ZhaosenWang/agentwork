package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// stewardSystemPrompt is the default persona shipped to the steward agent
// (小二) at seed time. The steward receives the owner's instructions,
// decomposes and delegates them to the right worker agent, tracks them to
// completion, and reports back faithfully.
const stewardSystemPrompt = "你是一位严谨周到的管家。职责：接收主人指令、拆解并委派给合适的执行者、跟进直到闭环、如实汇报结果。不确定时先问清需求，绝不擅自假设。回复简明扼要。"

// stewardDescription is the human-facing one-liner shown in the web agent
// list — what the steward does at a glance.
const stewardDescription = "系统内部解析 agent：team-import / intake 处理器"

// Agent is a runtime + a persona. Under the per-task connection model, creating
// an agent does NOT launch a process: a fresh connection is opened per run.
// status/pid columns are deliberately gone — they belong to the future
// long-lived-session model and would be dead columns today.
type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	// Type: standard (user-created, the default) | steward (system-internal
	// parser agent for team-import / intake; auto-seeded at daemon startup).
	Type         string            `json:"type"`
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
	if a.Type == "" {
		a.Type = "standard"
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.MaxConcurrent < 1 {
		a.MaxConcurrent = 3
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
		`INSERT INTO agent (id,name,type,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Type, a.Description, a.RuntimeID, a.SystemPrompt, a.Model, string(envJSON), string(mcpJSON), skillsJSON, a.MaxConcurrent, a.CreatedAt)
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
	var existingType string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT type FROM agent WHERE id=?`, id).Scan(&existingType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("check agent type: %w", err)
	}
	if a.Type == "" {
		a.Type = existingType
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
		a.MaxConcurrent = 3
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, id, "agent"); err != nil {
		return nil, err
	}
	envJSON, _ := json.Marshal(a.Env)
	mcpJSON, _ := json.Marshal(a.McpServers)
	skillsJSON, _ := json.Marshal(a.Skills)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE agent SET name=?, type=?, description=?, runtime_id=?, system_prompt=?, model=?, max_concurrent=?, env=?, mcp_servers=?, skills=? WHERE id=?`,
		a.Name, a.Type, a.Description, a.RuntimeID, a.SystemPrompt, a.Model, a.MaxConcurrent, string(envJSON), string(mcpJSON), string(skillsJSON), id); err != nil {
		if ve := dupNameCodedError(err, CodeAgentNameExists, "agent", a.Name); ve != nil {
			return nil, ve
		}
		return nil, fmt.Errorf("update agent: %w", err)
	}
	return s.Get(ctx, id)
}


// UpsertByName creates or updates an agent by name (the team-import path).
// When the agent exists, its description/system_prompt/skills are updated;
// runtime_id is left unchanged (the team repo does not define machines). When
// it does not exist, a new agent is created bound to the supplied runtimeID.
// Returns the agent.
func (s *AgentService) UpsertByName(ctx context.Context, name, description, systemPrompt, runtimeID string, skillIDs []string) (*Agent, error) {
	if name = strings.TrimSpace(name); name == "" {
		return nil, NewValidationError("agent name is required")
	}
	var existingID string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT id FROM agent WHERE name=?`, name).Scan(&existingID)
	if existingID != "" {
		if skillIDs == nil {
			skillIDs = []string{}
		}
		skillsJSON, _ := json.Marshal(skillIDs)
		if _, err := s.st.DB().ExecContext(ctx,
			`UPDATE agent SET description=?, system_prompt=?, skills=? WHERE id=?`,
			description, systemPrompt, string(skillsJSON), existingID); err != nil {
			return nil, fmt.Errorf("update agent %q: %w", name, err)
		}
		return s.Get(ctx, existingID)
	}
	if runtimeID == "" {
		return nil, NewValidationError(fmt.Sprintf("runtime_id is required for new agent %q", name))
	}
	a := Agent{
		Name:         name,
		Description:  description,
		RuntimeID:    runtimeID,
		SystemPrompt: systemPrompt,
		Skills:       skillIDs,
		MaxConcurrent: 1,
	}
	return s.Create(ctx, a)
}

func (s *AgentService) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,type,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at
		 FROM agent ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		var envJSON, mcpJSON, skillsJSON string
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.CreatedAt); err != nil {
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
		`SELECT id,name,type,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at
		 FROM agent WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Type, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.CreatedAt)
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
		// Sidebar pin preference: a user-facing preference, not a guard — drop
		// it silently. Must precede the agent delete: agent_pin.agent_id is a
		// non-cascading FK, so a leftover pin row would reject DELETE FROM agent.
		`DELETE FROM agent_pin WHERE agent_id=?`,
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

// GetSteward returns the system-internal steward agent (type=steward), used by
// the team-import flow to run the exploration processor task. Returns
// ErrNotFound if no steward agent exists (the daemon seed has not run or no
// active runtime was available at startup).
func (s *AgentService) GetSteward(ctx context.Context) (*Agent, error) {
	var id string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM agent WHERE type='steward' LIMIT 1`).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.Get(ctx, id)
}

// EnsureStewardRuntime checks whether the steward agent's runtime is still
// active. If not, it reassigns the steward to the first active runtime. Returns
// an error if no active runtime exists. Called at daemon startup (seed) and at
// import time (safety check).
func (s *AgentService) EnsureStewardRuntime(ctx context.Context) error {
	st, err := s.GetSteward(ctx)
	if err != nil {
		return err
	}
	var status string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT status FROM runtime WHERE id=?`, st.RuntimeID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NewValidationError("steward agent's runtime no longer exists")
		}
		return fmt.Errorf("check steward runtime: %w", err)
	}
	if status == "active" {
		return nil
	}
	activeID, err := s.firstActiveRuntimeID(ctx)
	if err != nil {
		return err
	}
	if _, err := s.st.DB().ExecContext(ctx, `UPDATE agent SET runtime_id=? WHERE id=?`, activeID, st.ID); err != nil {
		return fmt.Errorf("reassign steward runtime: %w", err)
	}
	return nil
}

// firstActiveRuntimeID returns the ID of the first active runtime by creation
// order, or an error if none exist.
func (s *AgentService) firstActiveRuntimeID(ctx context.Context) (string, error) {
	var id string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM runtime WHERE status='active' ORDER BY created_at LIMIT 1`).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NewValidationError("no active runtime available — connect a machine first")
		}
		return "", fmt.Errorf("find active runtime: %w", err)
	}
	return id, nil
}

// ListActiveRuntimeNames returns the names of all active runtimes, for the
// steward agent's import prompt (it decides which runtime each imported agent
// binds to).
func (s *AgentService) ListActiveRuntimeNames(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT name FROM runtime WHERE status='active' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ListActiveRuntimeIDs returns the IDs of all active runtimes, for resolving
// runtime names in team.json to runtime IDs during import ingestion.
func (s *AgentService) ListActiveRuntimeIDs(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id FROM runtime WHERE status='active' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SeedSteward inserts the system-internal steward agent if none exists. Called
// at daemon startup. The steward is bound to the first active runtime; if no
// active runtime exists, the seed is skipped (ImportTeam will report the error
// when the user tries to import). If the steward already exists but its
// runtime is no longer active, it is reassigned.
func (s *AgentService) SeedSteward(ctx context.Context) error {
	_, err := s.GetSteward(ctx)
	if err == nil {
		return s.EnsureStewardRuntime(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	activeID, err := s.firstActiveRuntimeID(ctx)
	if err != nil {
		// No active runtime yet — skip seed; ImportTeam will report the error.
		if errors.Is(err, ErrValidation) {
			return nil
		}
		return err
	}
	if err := s.insertSteward(ctx, activeID); err != nil {
		return fmt.Errorf("seed steward agent: %w", err)
	}
	return nil
}

// SeedStewardForRuntime seeds the steward agent bound to a specific runtime
// (by runtime name, e.g. "hwcloud@laptop"). Called when a machine registers
// or updates its probe with a hwcloud CLI — the steward is auto-created if
// it doesn't already exist. Idempotent: if a steward already exists (even on
// a different runtime), this is a no-op. Returns an error only if the named
// runtime doesn't exist or isn't active.
func (s *AgentService) SeedStewardForRuntime(ctx context.Context, runtimeName string) error {
	_, err := s.GetSteward(ctx)
	if err == nil {
		return nil // steward already exists — idempotent no-op
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	var runtimeID string
	err = s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM runtime WHERE name=? AND status='active'`, runtimeName).Scan(&runtimeID)
	if errors.Is(err, sql.ErrNoRows) {
		return NewValidationError(fmt.Sprintf("runtime %q not found or not active", runtimeName))
	}
	if err != nil {
		return fmt.Errorf("lookup runtime %s: %w", runtimeName, err)
	}
	if err := s.insertSteward(ctx, runtimeID); err != nil {
		return fmt.Errorf("seed steward agent for %s: %w", runtimeName, err)
	}
	return nil
}

// insertSteward inserts the system-internal steward agent bound to the given
// runtime. Shared by SeedSteward (first active runtime) and SeedStewardForRuntime
// (named runtime).
func (s *AgentService) insertSteward(ctx context.Context, runtimeID string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,type,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), "小二", "steward", stewardDescription, runtimeID, stewardSystemPrompt, "", "{}", "[]", "[]", 1, now())
	return err
}
