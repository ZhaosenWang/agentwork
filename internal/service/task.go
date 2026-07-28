package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Task is the unit of work. The ACP session id is server-generated and stored
// in the session table; it is not derived from the task id.
type Task struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	ParentID      string `json:"parent_id"`
	AssigneeType  string `json:"assignee_type"`
	AssigneeID    string `json:"assignee_id"`
	Status        string `json:"status"`
	HandoffNote   string `json:"handoff_note"`
	CreatedByType string `json:"created_by_type"`
	CreatedByID   string `json:"created_by_id"`
	CreatedAt     string `json:"created_at"`
}

type TaskService struct {
	st  *store.Store
	bus *events.Bus
}

func NewTaskService(st *store.Store, bus *events.Bus) *TaskService {
	return &TaskService{st: st, bus: bus}
}

// Create inserts a task. If assignee is an agent and status isn't backlog,
// enqueues a task_queue row so the daemon picks it up.
func (s *TaskService) Create(ctx context.Context, t Task) (*Task, error) {
	if t.Title == "" {
		return nil, NewValidationError("title is required")
	}
	if t.AssigneeType == "" {
		t.AssigneeType = "agent"
	}
	if t.Status == "" {
		t.Status = "backlog"
	}
	if t.CreatedByType == "" {
		t.CreatedByType = "human"
	}
	// Verify parent exists before insert — otherwise the FK on task.parent_id
	// fires and surfaces as a 500.
	if t.ParentID != "" {
		var n int
		if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM task WHERE id=?`, t.ParentID).Scan(&n); err != nil {
			return nil, fmt.Errorf("check parent: %w", err)
		}
		if n == 0 {
			return nil, NewValidationError(fmt.Sprintf("parent task %q does not exist", t.ParentID))
		}
	}
	// Verify assignee agent exists when assigning to an agent.
	if t.AssigneeType == "agent" && t.AssigneeID != "" {
		var n int
		if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE id=?`, t.AssigneeID).Scan(&n); err != nil {
			return nil, fmt.Errorf("check assignee: %w", err)
		}
		if n == 0 {
			return nil, NewValidationError(fmt.Sprintf("assignee agent %q does not exist", t.AssigneeID))
		}
	}
	t.ID = newID()
	t.CreatedAt = now()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var parentID any
	if t.ParentID != "" {
		parentID = t.ParentID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task (id,title,description,parent_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Description, parentID, t.AssigneeType, t.AssigneeID, t.Status, t.HandoffNote, t.CreatedByType, t.CreatedByID, t.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,task_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), t.ID, t.CreatedByType, t.CreatedByID, "created", "{}", t.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}

	if t.AssigneeType == "agent" && t.AssigneeID != "" && t.Status != "backlog" {
		if err := enqueueInTx(ctx, tx, t.ID, t.AssigneeID, t.CreatedAt); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.bus.Publish(ctx, events.Event{Topic: "task:created", Payload: t})
	return &t, nil
}

// enqueueInTx inserts a task_queue row and flips the task to "queued".
func enqueueInTx(ctx context.Context, tx *sql.Tx, taskID, agentID, ts string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_queue (id,task_id,agent_id,status,attempt,queued_at) VALUES (?,?,?,'queued',1,?)`,
		newID(), taskID, agentID, ts); err != nil {
		return fmt.Errorf("insert task_queue: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task SET status='queued' WHERE id=?`, taskID); err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	return nil
}

func (s *TaskService) List(ctx context.Context) ([]Task, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,title,description,parent_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at
		 FROM task ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var parentID sql.NullString
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &parentID, &t.AssigneeType, &t.AssigneeID, &t.Status, &t.HandoffNote, &t.CreatedByType, &t.CreatedByID, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.ParentID = parentID.String
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *TaskService) Get(ctx context.Context, id string) (*Task, error) {
	var t Task
	var parentID sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,title,description,parent_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at
		 FROM task WHERE id=?`, id).
		Scan(&t.ID, &t.Title, &t.Description, &parentID, &t.AssigneeType, &t.AssigneeID, &t.Status, &t.HandoffNote, &t.CreatedByType, &t.CreatedByID, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.ParentID = parentID.String
	return &t, nil
}

// Assign changes a task's assignee. If the new assignee is an agent, enqueues
// a run (this is the reassign/handoff path). The old session on the prior
// agent is frozen by the daemon when it notices the assignee changed.
func (s *TaskService) Assign(ctx context.Context, taskID, assigneeType, assigneeID, handoffNote string) (*Task, error) {
	t, err := s.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	// Verify the target agent exists before we enqueue — otherwise the FK
	// on task_queue.agent_id fires and surfaces as a 500.
	if assigneeType == "agent" && assigneeID != "" {
		var n int
		if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE id=?`, assigneeID).Scan(&n); err != nil {
			return nil, fmt.Errorf("check assignee: %w", err)
		}
		if n == 0 {
			return nil, NewValidationError(fmt.Sprintf("assignee agent %q does not exist", assigneeID))
		}
	}
	ts := now()
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE task SET assignee_type=?, assignee_id=?, handoff_note=? WHERE id=?`,
		assigneeType, assigneeID, handoffNote, taskID); err != nil {
		return nil, err
	}
	// Freeze the prior agent's active session for this task (handoff §1.3).
	// The new assignee's run will open a fresh session; the old session keeps
	// its history but no longer receives messages.
	if t.AssigneeType == "agent" && t.AssigneeID != "" && t.AssigneeID != assigneeID {
		if _, err := tx.ExecContext(ctx,
			`UPDATE session SET status='frozen' WHERE task_id=? AND agent_id=? AND status='active'`,
			taskID, t.AssigneeID); err != nil {
			return nil, fmt.Errorf("freeze old session: %w", err)
		}
	}
	detail, _ := json.Marshal(map[string]string{"from": t.AssigneeType + "/" + t.AssigneeID, "to": assigneeType + "/" + assigneeID})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,task_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'handoff',?,?)`,
		newID(), taskID, "human", "", string(detail), ts); err != nil {
		return nil, err
	}
	if assigneeType == "agent" && assigneeID != "" {
		if err := enqueueInTx(ctx, tx, taskID, assigneeID, ts); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	t.AssigneeType = assigneeType
	t.AssigneeID = assigneeID
	t.HandoffNote = handoffNote
	s.bus.Publish(ctx, events.Event{Topic: "task:assigned", Payload: t})
	return t, nil
}

// AddMessage appends one chat_message row to a task's history and publishes a
// task:message event. This is the structured side-effect channel for agents
// calling agentwork-cli `task message`.
func (s *TaskService) AddMessage(ctx context.Context, taskID, role, text string) error {
	if role == "" {
		role = "assistant"
	}
	_, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO chat_message (id,task_id,role,content,tool_calls,created_at) VALUES (?,?,?,?,'[]',?)`,
		newID(), taskID, role, text, now())
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "task:message", Payload: map[string]any{
		"task_id": taskID, "role": role, "text": text,
	}})
	return nil
}

// Cancel marks a non-terminal task as cancelled. Running tasks are not killed
// here — the daemon notices the status change and lets the current turn finish
// naturally; a future long-lived-session model could interrupt it. Queued
// tasks are dropped from task_queue. Already-terminal tasks are a no-op.
func (s *TaskService) Cancel(ctx context.Context, taskID string) (*Task, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE task SET status='cancelled' WHERE id=? AND status NOT IN ('completed','failed','cancelled')`,
		taskID)
	if err != nil {
		return nil, fmt.Errorf("cancel task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	// Drop any queued task_queue row — a cancelled task should not be
	// dispatched. A running row is left alone; the daemon's finishTask will
	// see the task is cancelled and not clobber it back.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM task_queue WHERE task_id=? AND status='queued'`, taskID); err != nil {
		return nil, fmt.Errorf("delete queued task_queue: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,task_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'cancelled','{}',?)`,
		newID(), taskID, "human", "", now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t, err := s.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	s.bus.Publish(ctx, events.Event{Topic: "task:finished", Payload: map[string]any{
		"task_id": taskID, "status": "cancelled", "summary": "",
	}})
	return t, nil
}

// Delete removes a task and all its dependents. Sub-tasks are orphaned
// (parent_id cleared) rather than recursively deleted — deleting a parent
// should not silently destroy children the caller may not know about.
func (s *TaskService) Delete(ctx context.Context, taskID string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM task_queue WHERE task_id=?`,
		`DELETE FROM session WHERE task_id=?`,
		`DELETE FROM chat_message WHERE task_id=?`,
		`DELETE FROM activity_log WHERE task_id=?`,
		`DELETE FROM schedule_run WHERE task_id=?`,
		`UPDATE task SET parent_id='' WHERE parent_id=?`,
		`DELETE FROM task WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, taskID); err != nil {
			return fmt.Errorf("delete task dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "task:deleted", Payload: map[string]string{"task_id": taskID}})
	return nil
}

// WaitChildren marks a task as waiting for its sub-tasks to finish. The agent
// calls this (via agentwork-cli `task wait`) after creating all the sub-tasks
// it wants to fan out. Idempotent: calling wait on a task that is already
// waiting_children is a no-op. Terminal tasks (completed/failed/cancelled)
// return ErrNotFound. The daemon later re-enqueues the task once every child
// reaches a terminal status.
func (s *TaskService) WaitChildren(ctx context.Context, taskID string) error {
	var status string
	err := s.st.DB().QueryRowContext(ctx, `SELECT status FROM task WHERE id=?`, taskID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	if status == "waiting_children" {
		return nil // already waiting; idempotent no-op
	}
	if status != "running" && status != "queued" {
		return ErrNotFound // terminal or invalid state
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE task SET status='waiting_children' WHERE id=?`, taskID); err != nil {
		return fmt.Errorf("wait-children: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,task_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'waiting_children','{}',?)`,
		newID(), taskID, "agent", "", now()); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "task:waiting", Payload: map[string]string{"task_id": taskID}})
	return nil
}

// childSummary is one sub-task's outcome, included in the wakeup prompt.
type childSummary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// WakeupParentIfReady is called after a child task reaches a terminal status.
// If the parent is waiting_children and all its children are now terminal, the
// parent is re-enqueued with a wakeup prompt summarising the children. If the
// parent isn't waiting, or children are still in flight, this is a no-op.
//
// The inflight check and the status flip happen in one transaction so a new
// child created between the check and the claim can't cause a premature wake:
// if inflight > 0 inside the tx, we return without flipping the parent.
func (s *TaskService) WakeupParentIfReady(ctx context.Context, childTaskID string) error {
	var parentID sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT parent_id FROM task WHERE id=?`, childTaskID).Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // child vanished; nothing to do
	}
	if err != nil {
		return fmt.Errorf("load parent_id: %w", err)
	}
	if !parentID.Valid || parentID.String == "" {
		return nil // no parent
	}

	// Quick pre-check outside the tx: if the parent isn't waiting or is
	// human-assigned, bail early without opening a transaction. The
	// authoritative guard is the WHERE status='waiting_children' inside the tx.
	var parentStatus, assigneeType, assigneeID string
	err = s.st.DB().QueryRowContext(ctx,
		`SELECT status, assignee_type, assignee_id FROM task WHERE id=?`, parentID.String).
		Scan(&parentStatus, &assigneeType, &assigneeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load parent: %w", err)
	}
	if parentStatus != "waiting_children" {
		return nil
	}
	if assigneeType != "agent" || assigneeID == "" {
		return nil // human-assigned parents stay for manual review
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Count non-terminal children and gather summaries in the same snapshot.
	// If a new child was created after our pre-check, inflight will be > 0
	// here and we bail without flipping the parent.
	var inflight int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task WHERE parent_id=? AND status NOT IN ('completed','failed','cancelled')`,
		parentID.String).Scan(&inflight); err != nil {
		return fmt.Errorf("count children: %w", err)
	}
	if inflight > 0 {
		return nil // still children running; don't wake
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, title, status, COALESCE((SELECT result_summary FROM task_queue WHERE task_id=task.id ORDER BY finished_at DESC LIMIT 1), '')
		 FROM task WHERE parent_id=? ORDER BY created_at`, parentID.String)
	if err != nil {
		return fmt.Errorf("load children: %w", err)
	}
	var children []childSummary
	for rows.Next() {
		var c childSummary
		if err := rows.Scan(&c.ID, &c.Title, &c.Status, &c.Summary); err != nil {
			rows.Close()
			return fmt.Errorf("scan child: %w", err)
		}
		children = append(children, c)
	}
	rows.Close()

	note := buildWakeupNote(children)
	detail, _ := json.Marshal(children)
	// Atomically claim the wakeup: only flip waiting_children→queued. If a
	// concurrent wakeup already flipped it, RowsAffected=0 and we bail — this
	// is what prevents two children finishing in parallel from double-enqueuing
	// the parent.
	res, err := tx.ExecContext(ctx,
		`UPDATE task SET status='queued', handoff_note=? WHERE id=? AND status='waiting_children'`,
		note, parentID.String)
	if err != nil {
		return fmt.Errorf("wake parent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // someone else woke it
	}
	if err := enqueueInTx(ctx, tx, parentID.String, assigneeID, now()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,task_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'children_done',?,?)`,
		newID(), parentID.String, "system", "", string(detail), now()); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	s.bus.Publish(ctx, events.Event{Topic: "task:wakeup", Payload: map[string]any{
		"parent_id": parentID.String, "children": children,
	}})
	return nil
}

// buildWakeupNote renders the child summaries into the handoff_note that
// buildPrompt appends to the parent's next run.
func buildWakeupNote(children []childSummary) string {
	var b strings.Builder
	b.WriteString("Sub-tasks complete:\n")
	for _, c := range children {
		fmt.Fprintf(&b, "- %s [%s]", c.Title, c.Status)
		if c.Summary != "" {
			b.WriteString(": ")
			b.WriteString(c.Summary)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
