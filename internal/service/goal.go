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

// Goal is a work item (the product plane). It is the SOLE holder of state
// authority: any change to its status flows through ReconcileOnRunEnd, which
// checks whether the reporting run still belongs to the current assignee
// before touching status. See DESIGN.zh.md §2/§7.
type Goal struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	AssigneeType  string `json:"assignee_type"` // agent | squad | human
	AssigneeID    string `json:"assignee_id"`
	Status        string `json:"status"`        // backlog|active|done|failed|cancelled
	HandoffNote   string `json:"handoff_note"`
	CreatedByType string `json:"created_by_type"` // human | agent
	CreatedByID   string `json:"created_by_id"`
	CreatedAt     string `json:"created_at"`
}

// goalRunContext is what ReconcileOnRunEnd reasons about. Carried separately
// so the reconciliation logic is testable without a live run row.
type goalRunContext struct {
	RunID       string
	GoalID      string
	AgentID     string
	IsLeaderRun bool
	SquadID     string
	Status      string // run's terminal status: completed|failed
	Attempt     int
	Summary     string
}

const maxAttempts = 3

type GoalService struct {
	st     *store.Store
	bus    *events.Bus
	runSvc *RunService // back-reference for retry/wake enqueue (same package)
}

func NewGoalService(st *store.Store, bus *events.Bus) *GoalService {
	return &GoalService{st: st, bus: bus}
}

// SetRunService wires the RunService back-reference once both exist. Kept
// explicit (not in the constructor) to avoid a constructor-order chicken/egg.
func (s *GoalService) SetRunService(rs *RunService) { s.runSvc = rs }

// Create inserts a goal. backlog (the semantic invariant) does not enqueue a
// run; an active assignee to an agent/squad does. Enqueuing itself happens in
// RunService to keep this method about goal rows + validation.
func (s *GoalService) Create(ctx context.Context, g Goal) (*Goal, error) {
	if g.Title == "" {
		return nil, NewValidationError("title is required")
	}
	if g.AssigneeType == "" {
		g.AssigneeType = "agent"
	}
	if g.Status == "" {
		g.Status = "backlog"
	}
	if g.CreatedByType == "" {
		g.CreatedByType = "human"
	}
	switch g.AssigneeType {
	case "agent":
		if g.AssigneeID == "" {
			return nil, NewValidationError("assignee_id is required for an agent goal")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, g.AssigneeID, "agent"); err != nil {
			return nil, err
		}
	case "squad":
		if g.AssigneeID == "" {
			return nil, NewValidationError("assignee_id is required for a squad goal")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, g.AssigneeID, "squad"); err != nil {
			return nil, err
		}
	case "human":
		// human-assigned goals have no agent run; assigning one is a manual placeholder.
	default:
		return nil, NewValidationError("assignee_type must be agent, squad, or human")
	}

	g.ID = newID()
	g.CreatedAt = now()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Title, g.Description, g.AssigneeType, g.AssigneeID, g.Status, g.HandoffNote, g.CreatedByType, g.CreatedByID, g.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert goal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), g.ID, g.CreatedByType, g.CreatedByID, "created", "{}", g.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.bus.Publish(ctx, events.Event{Topic: "goal:created", Payload: g})
	return &g, nil
}

func (s *GoalService) List(ctx context.Context) ([]Goal, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,title,description,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at
		 FROM goal ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		var g Goal
		if err := rows.Scan(&g.ID, &g.Title, &g.Description, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *GoalService) Get(ctx context.Context, id string) (*Goal, error) {
	var g Goal
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,title,description,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at
		 FROM goal WHERE id=?`, id).
		Scan(&g.ID, &g.Title, &g.Description, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// Assign changes a goal's assignee (the handoff path). Per DESIGN.zh.md §5.1
// this does NOT cancel in-flight runs: when an orphaned run later reports in,
// ReconcileOnRunEnd sees run.agent != goal.assignee and discards its result
// without touching goal.status. The new assignee's run is enqueued by the
// caller (RunService).
func (s *GoalService) Assign(ctx context.Context, goalID, assigneeType, assigneeID, handoffNote string) (*Goal, error) {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	switch assigneeType {
	case "agent":
		if assigneeID == "" {
			return nil, NewValidationError("assignee_id is required for an agent goal")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, assigneeID, "agent"); err != nil {
			return nil, err
		}
	case "squad":
		if assigneeID == "" {
			return nil, NewValidationError("assignee_id is required for a squad goal")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, assigneeID, "squad"); err != nil {
			return nil, err
		}
	case "human":
		// unassign / placeholder
	default:
		return nil, NewValidationError("assignee_type must be agent, squad, or human")
	}

	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET assignee_type=?, assignee_id=?, handoff_note=? WHERE id=?`,
		assigneeType, assigneeID, handoffNote, goalID); err != nil {
		return nil, fmt.Errorf("assign goal: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"from": g.AssigneeType + "/" + g.AssigneeID, "to": assigneeType + "/" + assigneeID})
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'handoff',?,?)`,
		newID(), goalID, "human", "", string(detail), now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}

	g.AssigneeType = assigneeType
	g.AssigneeID = assigneeID
	g.HandoffNote = handoffNote
	s.bus.Publish(ctx, events.Event{Topic: "goal:assigned", Payload: g})
	return g, nil
}

// Cancel marks a non-terminal goal cancelled. Like handoff, this does not kill
// in-flight runs: correctness comes from ReconcileOnRunEnd seeing goal.status
// cancelled and discarding the result.
func (s *GoalService) Cancel(ctx context.Context, goalID string) (*Goal, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='cancelled' WHERE id=? AND status NOT IN ('done','failed','cancelled')`,
		goalID)
	if err != nil {
		return nil, fmt.Errorf("cancel goal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	// Drop queued runs; running runs are left to report in (their result is
	// discarded by reconcile). A cancelled goal must not dispatch.
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
		return nil, fmt.Errorf("cancel queued runs: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'cancelled','{}',?)`,
		newID(), goalID, "human", "", now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:finished", Payload: map[string]any{
		"goal_id": goalID, "status": "cancelled", "summary": "",
	}})
	return s.Get(ctx, goalID)
}

// Delete removes a goal and dependents.
func (s *GoalService) Delete(ctx context.Context, goalID string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM chat_message WHERE run_id IN (SELECT id FROM run WHERE goal_id=?)`,
		`DELETE FROM run WHERE goal_id=?`,
		`DELETE FROM comment WHERE goal_id=?`,
		`DELETE FROM activity_log WHERE goal_id=?`,
		`DELETE FROM schedule_run WHERE goal_id=?`,
		`DELETE FROM goal WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, goalID); err != nil {
			return fmt.Errorf("delete goal dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:deleted", Payload: map[string]string{"goal_id": goalID}})
	return nil
}

// ownRunByGoal reports whether the reporting run still belongs to the goal's
// current assignee. This is the gate that makes handoff/cancel/reassign
// self-consistent without an external authority (DESIGN.zh.md §7).
//
//	agent-assigned goal: the run's agent must equal the goal's assignee.
//	squad-assigned goal: the run must be a leader run whose squad matches.
//	human-assigned goal: never has an agent-run owner — fall through to false.
func (s *GoalService) ownRunByGoal(ctx context.Context, tx *sql.Tx, rc goalRunContext, g Goal) (bool, error) {
	switch g.AssigneeType {
	case "agent":
		return rc.AgentID == g.AssigneeID && !rc.IsLeaderRun, nil
	case "squad":
		if !rc.IsLeaderRun || rc.SquadID != g.AssigneeID {
			return false, nil
		}
		// Squad leader run: always valid (team rules govern delegation).
		return true, nil
	default:
		return false, nil // human-assigned
	}
}

// ReconcileOnRunEnd is the ONLY place that advances a goal's status based on a
// run's terminal outcome. It is called by the daemon after a run reaches a
// terminal status (completed/failed). Everything else — handoff, cancel,
// wait-children — only changes goal.status directly; they never use a run's
// result. See DESIGN.zh.md §7.
//
// Rules:
//
//	run.agent != current assignee  → discard (handoff/reassign orphaned run)
//	goal.status == cancelled       → discard
//	run completed → goal → review (human approval required for done)
//	run failed, attempts left → enqueue a retry run (attempt+1)
//	run failed, attempts exhausted → goal → failed
func (s *GoalService) ReconcileOnRunEnd(ctx context.Context, rc goalRunContext) error {
	var pendingEvents []events.Event
	var afterCommit []func()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var g Goal
	err = tx.QueryRowContext(ctx,
		`SELECT id,assignee_type,assignee_id,status FROM goal WHERE id=?`, rc.GoalID).
		Scan(&g.ID, &g.AssigneeType, &g.AssigneeID, &g.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load goal for reconcile: %w", err)
	}

	owns, err := s.ownRunByGoal(ctx, tx, rc, g)
	if err != nil {
		return err
	}
	if !owns {
		pendingEvents = append(pendingEvents, events.Event{Topic: "run:discarded", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "orphaned",
		}})
		return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit)
	}
	if g.Status == "cancelled" {
		pendingEvents = append(pendingEvents, events.Event{Topic: "run:discarded", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "cancelled",
		}})
		return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit)
	}

	switch rc.Status {
	case "completed":
		// All completed runs push the goal to review (待审核). Only a
		// human can approve review → done. This applies uniformly to
		// single-agent goals and squad member runs.
		if res, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='review', handoff_note='' WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
			rc.GoalID); err != nil {
			return fmt.Errorf("promote goal to review: %w", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			break
		}

	case "failed":
		if rc.Attempt < maxAttempts {
			attempt := rc.Attempt + 1
			afterCommit = append(afterCommit, func() {
				if err := s.runSvc.EnqueueExisting(ctx, rc.GoalID, rc.AgentID, attempt, rc.IsLeaderRun, rc.SquadID); err != nil {
					s.bus.Publish(ctx, events.Event{Topic: "goal:retry_failed", Payload: map[string]any{
						"goal_id": rc.GoalID, "error": err.Error(),
					}})
				} else {
					s.bus.Publish(ctx, events.Event{Topic: "goal:retrying", Payload: map[string]any{
						"goal_id": rc.GoalID, "attempt": attempt,
					}})
				}
			})
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE goal SET status='failed' WHERE id=? AND status NOT IN ('done','failed','cancelled')`,
				rc.GoalID); err != nil {
				return fmt.Errorf("fail goal: %w", err)
			}
		}
	}

	pendingEvents = append(pendingEvents, events.Event{Topic: "goal:finished", Payload: map[string]any{
		"goal_id": rc.GoalID, "status": rc.Status, "summary": rc.Summary,
	}})
	return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit)
}

// SubmitForReview transitions the goal to review (待审核). Agents can only
// submit for review, not mark done directly. Only a human can approve or reject.
// Authorization: the current assignee's agent may submit.
func (s *GoalService) SubmitForReview(ctx context.Context, goalID, agentID, summary string) error {
	return s.markTerminal(ctx, goalID, agentID, "review", summary)
}

// MarkFailed transitions the goal to failed and posts a system comment.
func (s *GoalService) MarkFailed(ctx context.Context, goalID, agentID, summary string) error {
	return s.markTerminal(ctx, goalID, agentID, "failed", summary)
}

// Approve promotes a goal from review to done. Human-only.
func (s *GoalService) Approve(ctx context.Context, goalID, summary string) error {
	return s.humanReviewAction(ctx, goalID, "done", "approved", summary)
}

// Reject sends a goal back from review to active. Human-only.
func (s *GoalService) Reject(ctx context.Context, goalID, reason string) error {
	return s.humanReviewAction(ctx, goalID, "active", "rejected", reason)
}

func (s *GoalService) humanReviewAction(ctx context.Context, goalID, targetStatus, action, note string) error {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return err
	}
	if g.Status != "review" {
		return NewValidationError("goal must be in review status to " + action)
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET status=? WHERE id=?`, targetStatus, goalID); err != nil {
		return fmt.Errorf("%s goal: %w", action, err)
	}

	commentID := newID()
	ts := now()
	label := map[string]string{"done": "Approved.", "active": "Rejected. Reason: "}[targetStatus]
	content := label + note
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,?,?,?)`,
		commentID, goalID, "human", "", nil, content, ts); err != nil {
		return fmt.Errorf("insert comment: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), goalID, "human", "", action, "{}", ts); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	topic := "goal:finished"
	if targetStatus == "active" {
		topic = "goal:rejected"
	}
	s.bus.Publish(ctx, events.Event{Topic: topic, Payload: map[string]any{
		"goal_id": goalID, "status": targetStatus, "summary": content,
	}})
	s.bus.Publish(ctx, events.Event{Topic: "comment:created", Payload: Comment{
		ID: commentID, GoalID: goalID, AuthorType: "human",
		Content: content, CreatedAt: ts,
	}})
	return nil
}

func (s *GoalService) markTerminal(ctx context.Context, goalID, agentID, status, summary string) error {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return err
	}
	// Only the assigned agent (or squad leader) may submit for review / mark failed.
	if g.AssigneeType == "agent" && g.AssigneeID != agentID {
		return NewValidationError("only the current assignee may mark the goal as " + status)
	}
	if g.Status == "done" || g.Status == "failed" || g.Status == "cancelled" || g.Status == "review" {
		return NewValidationError("goal is already terminal or under review")
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET status=?, handoff_note='' WHERE id=?`, status, goalID); err != nil {
		return fmt.Errorf("mark goal %s: %w", status, err)
	}

	commentID := newID()
	ts := now()
	label := map[string]string{
		"review": "Goal submitted for review.",
		"failed": "Goal failed.",
	}[status]
	content := fmt.Sprintf("%s Summary: %s", label, summary)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,?,?,?)`,
		commentID, goalID, "system", "", nil, content, ts); err != nil {
		return fmt.Errorf("insert system comment: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), goalID, "agent", agentID, status, "{}", ts); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.bus.Publish(ctx, events.Event{Topic: "goal:finished", Payload: map[string]any{
		"goal_id": goalID, "status": status, "summary": summary,
	}})
	s.bus.Publish(ctx, events.Event{Topic: "comment:created", Payload: Comment{
		ID: commentID, GoalID: goalID, AuthorType: "system",
		Content: content, CreatedAt: ts,
	}})
	return nil
}

// mustExist is a tiny existence-check helper shared by the services.
func mustExist(ctx context.Context, st *store.Store, query, id, label string) error {
	var n int
	if err := st.DB().QueryRowContext(ctx, query, id).Scan(&n); err != nil {
		return fmt.Errorf("check %s: %w", label, err)
	}
	if n == 0 {
		return NewValidationError(fmt.Sprintf("%s %q does not exist", label, id))
	}
	return nil
}

// commitAndEmit commits the transaction and, only on success, publishes the
// collected events and runs the after-commit callbacks. This keeps the
// "bus.Publish after commit" invariant: a rolled-back transaction (commit
// error) emits nothing.
func (s *GoalService) commitAndEmit(ctx context.Context, tx *sql.Tx, evs []events.Event, after []func()) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, e := range evs {
		s.bus.Publish(ctx, e)
	}
	for _, f := range after {
		f()
	}
	return nil
}
