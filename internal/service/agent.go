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
	// ArchivedAt / ArchivedBy are the soft-archive markers (对齐 multica
	// archived_at). '' = active. Populated by Archive; an archived agent is
	// excluded from List but still returned by Get (audit rows that store the
	// bare id stay JOIN-resolvable to a name). See plan §4.
	ArchivedAt    string          `json:"archived_at"`
	ArchivedBy    string          `json:"archived_by"`
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
		if ve := dupNameError(err, a.Name); ve != nil {
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
		return nil, NewValidationError("name is required")
	}
	if a.RuntimeID == "" {
		return nil, NewValidationError("runtime_id is required")
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
		if ve := dupNameError(err, a.Name); ve != nil {
			return nil, ve
		}
		return nil, fmt.Errorf("update agent: %w", err)
	}
	return s.Get(ctx, id)
}

// dupNameError returns a 400 validation error ("agent %q already exists")
// when err is a SQLite UNIQUE-name conflict, or nil otherwise. Identified by
// the driver's extended error code SQLITE_CONSTRAINT_UNIQUE (2067) via
// errors.As — precise (excludes NOT NULL / FK / other constraints) and
// driver-typed. Callers return it directly on hit, else wrap the original
// err for a 500.
func dupNameError(err error, name string) error {
	var se interface{ Code() int }
	if errors.As(err, &se) && se.Code() == 2067 {
		return NewValidationError(fmt.Sprintf("agent %q already exists", name))
	}
	return nil
}

// List returns agents. Active only by default (archived rows excluded, plan
// §4.5); includeArchived=true returns archived rows too so the UI can render
// historical references with the agent's original name + a "已删除" tag.
// Get does NOT filter, so audit references stay resolvable regardless.
func (s *AgentService) List(ctx context.Context, includeArchived bool) ([]Agent, error) {
	q := `SELECT id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,archived_at,archived_by,created_at FROM agent`
	if !includeArchived {
		q += ` WHERE archived_at=''`
	}
	q += ` ORDER BY created_at`
	rows, err := s.st.DB().QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		var envJSON, mcpJSON, skillsJSON string
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.ArchivedAt, &a.ArchivedBy, &a.CreatedAt); err != nil {
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
	// NOTE: no archived_at filter — an archived agent stays readable by id so
	// audit rows (comment.author_id, activity_log.actor_id, ...) JOIN back to
	// a name. List is the active-only view; Get is the resolver.
	var a Agent
	var envJSON, mcpJSON, skillsJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,archived_at,archived_by,created_at
		 FROM agent WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.ArchivedAt, &a.ArchivedBy, &a.CreatedAt)
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

// Archive soft-deletes an agent (对齐 multica ArchiveAgent, plan §4): the row
// is marked archived_at instead of hard-deleted, so audit rows that store the
// bare agent id (comment.author_id, activity_log.actor_id, handoff_event.*,
// consult_request.*, sub_goal.assignee_id/verifier_id, goal.created_by_id)
// stay JOIN-resolvable to a name forever. History (runs, chat messages,
// comments) is preserved in place.
//
// Runtime cleanup mirrors goal:deleted — the agent's running runs are
// captured BEFORE the transaction and carried in the event payload; the
// daemon's onAgentArchived cancelRun's each one and drops the worker. The
// agent's owned goals fall back to human (an archived agent must not own
// active goals) and its non-terminal sub-goals are cancelled (same as the old
// hard-delete path — a dead assignee would stall them running forever). Its
// schedules are disabled (enabled=0, not deleted) so they stop firing but can
// be re-enabled if the agent is ever restored.
//
// archivedBy records who performed the archive (human id or "system"); it is
// carried in the event payload alongside the run_ids.
func (s *AgentService) Delete(ctx context.Context, id string) error {
	return s.Archive(ctx, id, "")
}

// Archive is the soft-delete entry point. archivedBy is the actor ("system"
// when invoked internally, the human id when invoked from the HTTP handler —
// passed through from the session once auth is wired).
func (s *AgentService) Archive(ctx context.Context, id, archivedBy string) error {
	// Refuse if this agent leads a squad — forcing the caller to fix the squad
	// first is safer than archiving a leader (squad.leader_id is RESTRICT and
	// a leaderless squad is invalid). Reassign the squad's leader first.
	var squadCount int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM squad WHERE leader_id=?`, id).Scan(&squadCount); err != nil {
		return fmt.Errorf("check leader role: %w", err)
	}
	if squadCount > 0 {
		return NewValidationError(fmt.Sprintf("agent %s leads %d squad(s); delete or reassign them first", id, squadCount))
	}

	// Refuse if this agent has running runs — cutting a live process mid-run
	// is destructive and surprising. The operator stops or reassigns the runs
	// first, then archives. (Guard基调对齐 domain.Delete 的 schedule/processor
	// 守卫.) The daemon's onAgentArchived cancelRun path stays as a defensive
	// backstop for any race that lands a run after this check.
	var runningCount int
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE agent_id=? AND status='running'`, id).Scan(&runningCount); err != nil {
		return fmt.Errorf("check running runs: %w", err)
	}
	if runningCount > 0 {
		return NewValidationError(fmt.Sprintf("agent %s has %d running run(s); stop or reassign them first", id, runningCount))
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// squad_member rows are PRESERVED (not deleted) — an archived agent stays
	// on the roster with a "已删除" tag, so a squad's history is intact and
	// restore is trivial. Completeness at assign-time is judged by joining
	// agent.archived_at (see mustActiveSquadComplete), not by the row's
	// presence. Disabling schedules is enough to keep the agent off new work.
	if _, err := tx.ExecContext(ctx, `UPDATE schedule SET enabled=0 WHERE assignee_type='agent' AND assignee_id=?`, id); err != nil {
		return fmt.Errorf("disable schedules: %w", err)
	}
	// Cancel the agent's non-terminal sub-goals (terminal history stays) — a
	// deleted assignee must not leave work items that later enqueue a run on a
	// dead agent id. Same as the old hard-delete path.
	if _, err := tx.ExecContext(ctx,
		`UPDATE sub_goal SET status='cancelled' WHERE (assignee_id=?1 OR verifier_id=?1) AND status NOT IN ('verified','cancelled','failed')`, id); err != nil {
		return fmt.Errorf("cancel sub-goals: %w", err)
	}
	// The agent's owned goals fall back to human — an archived agent must not
	// own active goals. (NOT transferred: there's no natural successor for an
	// agent's goals, unlike a squad whose leader inherits them.)
	if _, err := tx.ExecContext(ctx, `UPDATE goal SET assignee_type='human', assignee_id='' WHERE assignee_type='agent' AND assignee_id=?`, id); err != nil {
		return fmt.Errorf("orphan agent goals: %w", err)
	}
	// Mark archived (the row stays — history + audit resolvability preserved).
	stamp := now()
	if _, err := tx.ExecContext(ctx, `UPDATE agent SET archived_at=?, archived_by=? WHERE id=?`, stamp, archivedBy, id); err != nil {
		return fmt.Errorf("archive agent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Tell the daemon to drop the agent's worker. run_ids is empty — the
	// running-run guard above refuses any active run, so onAgentArchived's
	// cancelRun loop is a no-op backstop (kept for race safety). Payload shape
	// mirrors goal:deleted ({run_ids}) for consistency.
	s.bus.Publish(ctx, events.Event{Topic: "agent:archived", Payload: map[string]any{
		"id":          id,
		"run_ids":     []string{},
		"archived_by": archivedBy,
	}})
	return nil
}
