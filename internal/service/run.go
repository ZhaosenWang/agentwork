package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Run is one execution of a goal by one agent (the execution plane). It has
// NO authority over goal status: on a terminal status the daemon calls
// GoalService.ReconcileOnRunEnd, which is the sole path that advances a goal.
// See DESIGN.zh.md §2/§7.
type Run struct {
	ID              string `json:"id"`
	GoalID          string `json:"goal_id"`
	AgentID         string `json:"agent_id"`
	SessionID       string `json:"session_id"`
	Workdir         string `json:"workdir"`
	Status          string `json:"status"`
	Attempt         int    `json:"attempt"`
	ResultSummary   string `json:"result_summary"`
	TriggerCommentID string `json:"trigger_comment_id"`
	IsLeaderRun     bool   `json:"is_leader_run"`
	SquadID         string `json:"squad_id"`
	QueuedAt        string `json:"queued_at"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
	CreatedAt       string `json:"created_at"`
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
// for the per-(goal,agent) pending coalesce (DESIGN.zh.md §9.5).
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
// the same tx so it sees the caller's un-committed state (this keeps
// assign + enqueue as one atomic operation).
func (s *RunService) enqueueTx(ctx context.Context, tx *sql.Tx, goalID, agentID string, attempt int, isLeader bool, squadID, triggerCommentID string) (*Run, error) {
	if pending, err := s.hasPending(ctx, tx, goalID, agentID); err != nil {
		return nil, err
	} else if pending {
		// Coalesce: a queued/running run for this (goal,agent) already exists
		// (possibly just advanced to active by this same tx). Don't duplicate.
		var id string
		_ = tx.QueryRowContext(ctx,
			`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND status IN ('queued','running') ORDER BY queued_at LIMIT 1`,
			goalID, agentID).Scan(&id)
		s.bus.Publish(ctx, events.Event{Topic: "run:coalesced", Payload: map[string]any{
			"goal_id": goalID, "agent_id": agentID,
		}})
		return &Run{ID: id, GoalID: goalID, AgentID: agentID}, nil
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
		`INSERT INTO run (id,goal_id,agent_id,session_id,workdir,status,attempt,result_summary,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.GoalID, r.AgentID, "", "", r.Status, r.Attempt, r.ResultSummary, r.TriggerCommentID, leaderFlag, r.SquadID, r.QueuedAt, r.StartedAt, r.FinishedAt, r.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "run:enqueued", Payload: r})
	return &r, nil
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
// Per DESIGN.zh.md §5.3 this enqueues on the mentioned agent (NOT the goal's
// current assignee) and does NOT cancel any in-flight run.
func (s *RunService) EnqueueForMention(ctx context.Context, goalID, agentID, triggerCommentID string) (*Run, error) {
	squadID := resolveGoalSquad(ctx, s.st, goalID)
	return s.enqueue(ctx, goalID, agentID, 1, false, squadID, triggerCommentID)
}

// resolveGoalSquad returns the squad ID if this goal was originally assigned
// to a squad (determined by inspecting past leader runs), otherwise "".
func resolveGoalSquad(ctx context.Context, st *store.Store, goalID string) string {
	var squadID string
	_ = st.DB().QueryRowContext(ctx,
		`SELECT squad_id FROM run WHERE goal_id=? AND is_leader_run=1 AND squad_id!='' ORDER BY queued_at DESC LIMIT 1`,
		goalID).Scan(&squadID)
	return squadID
}

func (s *RunService) enqueue(ctx context.Context, goalID, agentID string, attempt int, isLeader bool, squadID, triggerCommentID string) (*Run, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.enqueueTx(ctx, tx, goalID, agentID, attempt, isLeader, squadID, triggerCommentID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// EnqueueExistingTx is the same-package atomic variant for a caller that
// already holds a transaction.
func (s *RunService) EnqueueExistingTx(ctx context.Context, tx *sql.Tx, goalID, agentID string, attempt int, isLeader bool, squadID string) (*Run, error) {
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
// Per DESIGN.zh.md §7 the claim avoids the old global head-of-line blocking by
// letting the daemon pass the set of agents with free capacity and claiming
// only within that set. Returns (nil, nil) when nothing is claimable.
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
	var leaderFlag int
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, goal_id, agent_id, is_leader_run, squad_id, status, attempt, result_summary
		 FROM run WHERE id=?`, runID).
		Scan(&rc.RunID, &rc.GoalID, &rc.AgentID, &leaderFlag, &rc.SquadID, &rc.Status, &rc.Attempt, &rc.Summary)
	rc.IsLeaderRun = leaderFlag != 0
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

func (s *RunService) List(ctx context.Context, goalID string) ([]Run, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,goal_id,agent_id,session_id,workdir,status,attempt,result_summary,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at
		 FROM run WHERE goal_id=? ORDER BY queued_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var leaderFlag int
		if err := rows.Scan(&r.ID, &r.GoalID, &r.AgentID, &r.SessionID, &r.Workdir, &r.Status, &r.Attempt, &r.ResultSummary, &r.TriggerCommentID, &leaderFlag, &r.SquadID, &r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.IsLeaderRun = leaderFlag != 0
		out = append(out, r)
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