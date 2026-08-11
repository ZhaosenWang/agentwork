package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Run is one execution of a goal by one agent (the execution plane). It has
// NO authority over goal status: on a terminal status the daemon calls
// GoalService.ReconcileOnRunEnd, which is the sole path that advances a goal.
// See DESIGN.md §9.
type Run struct {
	ID               string `json:"id"`
	GoalID           string `json:"goal_id"`
	AgentID          string `json:"agent_id"`
	RunKind          string `json:"run_kind"` // worker|processor (platform-internal)
	RunType          string `json:"run_type"` // processor tasks: compile|intake (M3)
	DomainID         string `json:"domain_id"`
	Prompt           string `json:"prompt"` // processor runs only
	SessionID        string `json:"session_id"`
	Workdir          string `json:"workdir"`
	Status           string `json:"status"`
	Attempt          int    `json:"attempt"`
	ResultSummary    string `json:"result_summary"`
	Evidence         string `json:"evidence"` // JSON: diff stats + verify output + summary
	TriggerCommentID string `json:"trigger_comment_id"`
	IsLeaderRun      bool   `json:"is_leader_run"`
	SquadID          string `json:"squad_id"`
	QueuedAt         string `json:"queued_at"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	CreatedAt        string `json:"created_at"`
}

type RunService struct {
	st      *store.Store
	bus     *events.Bus
	goalSvc *GoalService
}

func NewRunService(st *store.Store, bus *events.Bus) *RunService {
	return &RunService{st: st, bus: bus}
}

// SetGoalService wires the back-reference once both exist. GoalService needs
// runSvc for retry/wake enqueue; RunService needs goalSvc for reconcile.
func (s *RunService) SetGoalService(gs *GoalService) { s.goalSvc = gs }

// resolveLeader returns the agent id that should run a goal assigned to a
// squad (always the squad's leader). For agent/human goals the assignee agent
// is used directly (or empty for human / an unmatched case).
func (s *RunService) resolveLeader(ctx context.Context, assigneeType, assigneeID string) (agentID string, isLeader bool, squadID string, err error) {
	switch assigneeType {
	case "agent":
		return assigneeID, false, "", nil
	case "squad":
		var leader string
		if e := s.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leader); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return "", false, "", NewValidationError("squad not found")
			}
			return "", false, "", fmt.Errorf("load squad leader: %w", e)
		}
		return leader, true, assigneeID, nil
	default:
		return "", false, "", NewValidationError("goal assignee is not an agent or squad")
	}
}

// hasPending reports whether goalID already has a queued/running run on agentID,
// for the per-(goal,agent) pending coalesce (DESIGN.md).
func (s *RunService) hasPending(ctx context.Context, tx *sql.Tx, goalID, agentID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND status IN ('queued','running')`,
		goalID, agentID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// enqueueTx is the atomic enqueue core: inserts a run row under the caller's
// transaction. It performs the per-(goal,agent) pending coalesce check INSIDE
// the same tx so it sees the caller's un-committed state (this is what keeps
// a parent-wake flipping blocked→active + enqueue one atomic operation, and
// what stops two parallel child-dones from double-enqueuing the parent).
//
// The run event (enqueued/coalesced) is RETURNED, not published: publishing
// inside the tx would violate the "bus.Publish after commit" invariant (a
// rolled-back tx must not emit). The caller publishes after its commit.
// Note: EnqueueExistingTx (the parent-wake path inside a goal tx) does not
// publish — the goal layer's commitAndEmit covers goal events; the run event
// for that path is a known M2 refinement.
func (s *RunService) enqueueTx(ctx context.Context, tx *sql.Tx, goalID, agentID string, attempt int, isLeader bool, squadID, triggerCommentID string) (*Run, *events.Event, error) {
	if pending, err := s.hasPending(ctx, tx, goalID, agentID); err != nil {
		return nil, nil, err
	} else if pending {
		// Coalesce: a queued/running run for this (goal,agent) already exists
		// (possibly just advanced to active by this same tx). Don't duplicate.
		var id string
		_ = tx.QueryRowContext(ctx,
			`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND status IN ('queued','running') ORDER BY queued_at LIMIT 1`,
			goalID, agentID).Scan(&id)
		ev := &events.Event{Topic: "run:coalesced", Payload: map[string]any{
			"goal_id": goalID, "agent_id": agentID,
		}}
		return &Run{ID: id, GoalID: goalID, AgentID: agentID}, ev, nil
	}
	ts := now()
	leaderFlag := 0
	if isLeader {
		leaderFlag = 1
	}
	r := Run{
		ID:              newID(),
		GoalID:          goalID,
		AgentID:         agentID,
		Attempt:         attempt,
		IsLeaderRun:     isLeader,
		SquadID:         squadID,
		TriggerCommentID: triggerCommentID,
		Status:          "queued",
		QueuedAt:        ts,
		CreatedAt:       ts,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,domain_id,session_id,workdir,status,attempt,result_summary,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.GoalID, r.AgentID, "worker", "", "", "", "", r.Status, r.Attempt, r.ResultSummary, r.TriggerCommentID, leaderFlag, r.SquadID, r.QueuedAt, r.StartedAt, r.FinishedAt, r.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert run: %w", err)
	}
	ev := &events.Event{Topic: "run:enqueued", Payload: r}
	return &r, ev, nil
}

// EnqueueForGoal creates the first run for a goal based on its current
// assignee. Idempotent: if a pending run already exists for this (goal,agent),
// it coalesces (returns it as a no-op success). Caller uses this when a goal
// moves into active state (assign, activate, create-with-status=active).
func (s *RunService) EnqueueForGoal(ctx context.Context, g Goal) (*Run, error) {
	if g.AssigneeType != "agent" && g.AssigneeType != "squad" {
		return nil, nil // human-assigned: no run
	}
	agentID, isLeader, squadID, err := s.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
	if err != nil {
		return nil, err
	}
	return s.enqueue(ctx, g.ID, agentID, 1, isLeader, squadID, "")
}

// EnqueueExisting enqueues a run on an explicit agent (used by retry and
// parent-wake: the agent id is already known). Coalesces if a pending run
// exists for this (goal,agent).
func (s *RunService) EnqueueExisting(ctx context.Context, goalID, agentID string, attempt int, isLeader bool, squadID string) error {
	_, err := s.enqueue(ctx, goalID, agentID, attempt, isLeader, squadID, "")
	return err
}

// EnqueueForMention creates a run on an explicitly-mentioned agent for the
// same goal, sourced from a comment. trigger_comment_id records provenance.
// Per DESIGN.md §2 this enqueues on the mentioned agent (NOT the goal's
// current assignee) and does NOT cancel any in-flight run.
func (s *RunService) EnqueueForMention(ctx context.Context, goalID, agentID, triggerCommentID string) (*Run, error) {
	return s.enqueue(ctx, goalID, agentID, 1, false, "", triggerCommentID)
}

// EnqueueProcessorRun creates a platform-internal processor run (DESIGN.md
// §8): no goal, a fixed prompt, associated with a domain being processed
// (compile) or carrying the platform context for the task (intake, M3).
// runType discriminates the processor task (compile|intake); the daemon's
// runProcessorTask dispatches on it. Coalesces: if the same (run_type,
// domain/agent) already has a queued/running processor run, it is returned
// instead of duplicating (a second compile of the same domain would race the
// first; a backlog of intake runs on the same agent would duplicate work).
func (s *RunService) EnqueueProcessorRun(ctx context.Context, runType, domainID, agentID, prompt string) (*Run, error) {
	if runType == "" {
		runType = "compile" // default: the original processor task
	}
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM run WHERE run_kind='processor' AND run_type=? AND domain_id=? AND agent_id=? AND status IN ('queued','running') LIMIT 1`,
		runType, domainID, agentID).Scan(&existing)
	if err == nil {
		return &Run{ID: existing, DomainID: domainID, AgentID: agentID, Status: "queued"}, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check pending processor run: %w", err)
	}

	ts := now()
	r := Run{
		ID:       newID(),
		DomainID: domainID,
		AgentID:  agentID,
		RunKind:  "processor",
		RunType:  runType,
		Prompt:   prompt,
		Status:   "queued",
		QueuedAt: ts,
		CreatedAt: ts,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,domain_id,prompt,session_id,workdir,status,attempt,result_summary,evidence,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, "", r.AgentID, r.RunKind, r.RunType, r.DomainID, r.Prompt, "", "", r.Status, 1, "", "", "", 0, "", r.QueuedAt, "", "", r.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert processor run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *RunService) enqueue(ctx context.Context, goalID, agentID string, attempt int, isLeader bool, squadID, triggerCommentID string) (*Run, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, ev, err := s.enqueueTx(ctx, tx, goalID, agentID, attempt, isLeader, squadID, triggerCommentID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Publish only after commit (invariant 13): a rolled-back tx emits nothing.
	if ev != nil {
		s.bus.Publish(ctx, *ev)
	}
	return r, nil
}

// EnqueueExistingTx is the same-package atomic variant for a caller that
// already holds a transaction (e.g. the parent-wake path in GoalService, which
// flips blocked→active and enqueues in one tx). The run event is RETURNED —
// the caller's tx owns the publish-after-commit contract and publishes after
// its commit (invariant 13).
func (s *RunService) EnqueueExistingTx(ctx context.Context, tx *sql.Tx, goalID, agentID string, attempt int, isLeader bool, squadID string) (*Run, *events.Event, error) {
	return s.enqueueTx(ctx, tx, goalID, agentID, attempt, isLeader, squadID, "")
}

// ClaimedRow is a claimed run row handed to the daemon's runTask.
type ClaimedRow struct {
	RunID   string
	GoalID  string
	AgentID string
	Attempt int
}

// Claim atomically claims the oldest queued run for one of the ready
// (has-worker, not crashed/deleted) agents AND has a free concurrency slot.
// Per DESIGN.md the claim avoids the old global head-of-line blocking by
// letting the daemon pass the set of agents with free capacity and claiming
// only within that set. Returns (nil, nil) when nothing is claimable.
//
// PER-GOAL SERIALIZATION: a goal's runs are strictly sequential — a queued
// run is not claimed while ANOTHER run of the same goal is running (the
// worktree is exclusive to one run at a time; a mention-triggered run
// arriving mid-run must WAIT, not race the worktree — the worktree-cleanliness
// gate used to cancel it instead, which silently dropped the review step).
// Processor runs (goal_id='') are unaffected.
func (s *RunService) Claim(ctx context.Context, readyAgents []string) (*ClaimedRow, error) {
	if len(readyAgents) == 0 {
		return nil, nil
	}
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// IN-list for the ready agents.
	placeholders, args := inPlaceholders(readyAgents)
	var r ClaimedRow
	err = tx.QueryRowContext(ctx,
		`UPDATE run
		 SET status='running', started_at=?
		 WHERE id = (
		   SELECT r.id FROM run r
		   JOIN agent a ON a.id = r.agent_id
		   WHERE r.status='queued' AND r.agent_id IN (`+placeholders+`)
		     AND NOT EXISTS (
		       SELECT 1 FROM run r2
		       WHERE r2.goal_id != '' AND r2.goal_id = r.goal_id AND r2.status='running'
		     )
		   ORDER BY r.queued_at
		   LIMIT 1
		 )
		 RETURNING id, goal_id, agent_id, attempt`, append([]any{now()}, args...)...).
		Scan(&r.RunID, &r.GoalID, &r.AgentID, &r.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}

// RecoverStuckRunning reclaims runs left 'running' by a previous daemon that
// died without finishing them. Reset to queued so dispatch re-claims them.
// Note: attempt is preserved — a run on its last attempt that the daemon lost
// still has its remaining retry credit (DELTA from multica: their HandleFailedTasks
// resets to todo; here we just keep the run queued, attempt unchanged).
func (s *RunService) RecoverStuckRunning(ctx context.Context) (int, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='queued', started_at='' WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkSession stamps the protocol-returned session id once runTask has opened
// a session (for history / future long-lived resume).
func (s *RunService) MarkSession(ctx context.Context, runID, sessionID, workdir string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET session_id=?, workdir=? WHERE id=?`, sessionID, workdir, runID)
	return err
}

// Finish records a run's terminal status + summary and reconciles the goal.
// This is the daemon's single chokepoint to end a run: it writes the run row
// then hands the outcome to the goal layer for authoritative state change.
func (s *RunService) Finish(ctx context.Context, runID, status, summary string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status=?, result_summary=?, finished_at=? WHERE id=?`,
		status, summary, now(), runID); err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	// Re-read the minimal context the goal layer needs to reconcile.
	var rc goalRunContext
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, goal_id, agent_id, is_leader_run, squad_id, status, attempt, result_summary, trigger_comment_id
		 FROM run WHERE id=?`, runID).
		Scan(&rc.RunID, &rc.GoalID, &rc.AgentID, &rc.IsLeaderRun, &rc.SquadID, &rc.Status, &rc.Attempt, &rc.Summary, &rc.TriggerCommentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load run for reconcile: %w", err)
	}
	if s.goalSvc == nil {
		return errors.New("runSvc.goalSvc not wired")
	}
	return s.goalSvc.ReconcileOnRunEnd(ctx, rc)
}

// NotifyChildDone is called after a sub-goal reaches a terminal status. It
// delegates to the goal layer's parent-wake logic (which checks the parent is
// blocked and all sub-goals terminal, then re-queues the parent's assignee).
// Kept on RunService for convenience; the real guard is in GoalService.
func (s *RunService) NotifyChildDone(ctx context.Context, childGoalID string) {
	if s.goalSvc == nil {
		return
	}
	if err := s.goalSvc.NotifyChildDone(ctx, childGoalID); err != nil {
		log.Printf("run: notify child-done for goal %s: %v", childGoalID, err)
	}
}

func (s *RunService) List(ctx context.Context, goalID string) ([]Run, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,goal_id,agent_id,run_kind,run_type,domain_id,session_id,workdir,status,attempt,result_summary,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at
		 FROM run WHERE goal_id=? ORDER BY queued_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var leaderFlag int
		if err := rows.Scan(&r.ID, &r.GoalID, &r.AgentID, &r.RunKind, &r.RunType, &r.DomainID, &r.SessionID, &r.Workdir, &r.Status, &r.Attempt, &r.ResultSummary, &r.TriggerCommentID, &leaderFlag, &r.SquadID, &r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.IsLeaderRun = leaderFlag != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunMessage is one row of a run's interaction stream (chat_message) — the
// Web run detail's "what is the agent doing right now" view.
type RunMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls string `json:"tool_calls"`
	CreatedAt string `json:"created_at"`
}

// ListMessages returns the run's persisted interaction stream, oldest first.
func (s *RunService) ListMessages(ctx context.Context, runID string) ([]RunMessage, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT role, content, tool_calls, created_at FROM chat_message WHERE run_id=? ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunMessage{}
	for rows.Next() {
		var m RunMessage
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCalls, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// inPlaceholders builds "?,?,?" for a slice and returns the args slice.
func inPlaceholders(ids []string) (string, []any) {
	ph := ""
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args[i] = id
	}
	return ph, args
}