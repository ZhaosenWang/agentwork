package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Goal is a work item (the product plane). It is the SOLE holder of state
// authority: any change to its status flows through ReconcileOnRunEnd, which
// checks whether the reporting run still belongs to the current assignee
// before touching status. See DESIGN.md §9.
type Goal struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	ParentID        string `json:"parent_id"`
	DomainID        string `json:"domain_id"`     // owning domain (required for agent/squad goals — v2)
	AssigneeType    string `json:"assignee_type"` // agent | squad | human
	AssigneeID      string `json:"assignee_id"`
	Status          string `json:"status"` // backlog|active|done|failed|blocked|review|cancelled
	HandoffNote     string `json:"handoff_note"`
	ReviewRequest   string `json:"review_request"`   // gate trigger reason / deliver-failure note
	HumanIterations int    `json:"human_iterations"` // reject iterations (separate from run.attempt)
	CreatedByType   string `json:"created_by_type"`  // human | agent
	CreatedByID     string `json:"created_by_id"`
	CreatedAt       string `json:"created_at"`
	WakeCount       int    `json:"wake_count"` // bumped each blocked→active wakeup; bounded to break runaway re-fan-out
	SourceRef       string `json:"source_ref"` // external source (M4-B): "github:owner/repo#123"
	// CurrentAgentID is the agent of the goal's latest running/queued run —
	// the list card's "who is working right now" ('' = nobody in flight).
	CurrentAgentID string `json:"current_agent_id"`
}

// goalRunContext is what ReconcileOnRunEnd reasons about. Carried separately
// so the reconciliation logic is testable without a live run row.
type goalRunContext struct {
	RunID            string
	GoalID           string
	AgentID          string
	IsLeaderRun      bool
	SquadID          string
	Status           string // run's terminal status: completed|failed
	Attempt          int
	Summary          string
	TriggerCommentID string // mention/协作来源（guest run 失败留痕用）
}

const maxAttempts = 3

// maxWakeCycles bounds how many blocked→active wakeups a parent goal may take
// before it is force-failed. This is the state-machine guard against runaway
// re-fan-out loops: if an agent, on a wakeup turn, keeps creating sub-goals +
// waiting again, the goal would otherwise cycle forever. The bound turns an
// infinite loop into a bounded, surfaced failure (mirrors the maxAttempts
// philosophy for transient run failures). Per DESIGN §9 — the loop guard is a
// state-machine invariant, not a prompt-measured "please don't redo" hope.
const maxWakeCycles = 3

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
	// Note: blocked/review are transitive statuses (WaitChildren / the
	// checkpoint gate); reject them at creation so callers can't plant a goal
	// in a waiting state.
	if g.Status == "blocked" || g.Status == "review" {
		return nil, NewValidationError("cannot create a goal in blocked/review status")
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
	// v2: agent/squad-executed goals must belong to a domain — the domain owns
	// the worktree and the acceptance policy (DESIGN.md §2). Human/backlog
	// goals may be domain-less.
	if g.AssigneeType != "human" {
		if g.DomainID == "" {
			return nil, NewValidationError("domain_id is required for an agent/squad goal")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM domain WHERE id=?`, g.DomainID, "domain"); err != nil {
			return nil, err
		}
	}
	if g.ParentID != "" {
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM goal WHERE id=?`, g.ParentID, "parent goal"); err != nil {
			return nil, err
		}
	}

	g.ID = newID()
	g.CreatedAt = now()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var parentID, domainID any
	if g.ParentID != "" {
		parentID = g.ParentID
	}
	if g.DomainID != "" {
		domainID = g.DomainID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,parent_id,domain_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at,source_ref)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Title, g.Description, parentID, domainID, g.AssigneeType, g.AssigneeID, g.Status, g.HandoffNote, g.CreatedByType, g.CreatedByID, g.CreatedAt, g.SourceRef); err != nil {
		return nil, fmt.Errorf("insert goal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), g.ID, g.CreatedByType, g.CreatedByID, "created", "{}", g.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	// The creation instruction lands in the comment feed AS A MENTION — the
	// same coordination shape an agent produces: [@Name](mention://agent|squad
	// /<id>) + instruction. Assigning a goal IS the first mention, so the
	// feed's timeline is uniform: "@dev-team 给 test-repo 添加 …" renders as a
	// highlighted chip, exactly like an agent-to-agent handoff. Written
	// directly (not via CommentService.Create) so creation never
	// dispatch-triggers: the assignee's run is enqueued by the caller, and a
	// description that mentions other agents must not double-trigger at
	// creation.
	if g.Description != "" && (g.AssigneeType == "agent" || g.AssigneeType == "squad") {
		label, err := s.assigneeLabel(ctx, tx, g.AssigneeType, g.AssigneeID)
		if err != nil {
			return nil, fmt.Errorf("resolve assignee label: %w", err)
		}
		content := "[@" + label + "](mention://" + g.AssigneeType + "/" + g.AssigneeID + ") " + g.Description
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,NULL,?,?)`,
			newID(), g.ID, g.CreatedByType, g.CreatedByID, content, g.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert creation comment: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.bus.Publish(ctx, events.Event{Topic: "goal:created", Payload: g})
	return &g, nil
}

func (s *GoalService) List(ctx context.Context) ([]Goal, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT g.id,g.title,g.description,g.parent_id,g.domain_id,g.assignee_type,g.assignee_id,g.status,g.handoff_note,g.review_request,g.human_iterations,g.created_by_type,g.created_by_id,g.created_at,g.wake_count,g.source_ref,
		        (SELECT r.agent_id FROM run r WHERE r.goal_id = g.id AND r.status IN ('running','queued') ORDER BY r.created_at DESC LIMIT 1)
		 FROM goal g ORDER BY g.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Goal{}
	for rows.Next() {
		var g Goal
		var parentID, domainID, currentAgent sql.NullString
		if err := rows.Scan(&g.ID, &g.Title, &g.Description, &parentID, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.WakeCount, &g.SourceRef, &currentAgent); err != nil {
			return nil, err
		}
		g.ParentID = parentID.String
		g.DomainID = domainID.String
		g.CurrentAgentID = currentAgent.String
		out = append(out, g)
	}
	return out, rows.Err()
}

// TimelineItem is one event in a goal's execution flow: a run segment
// (an agent's turn), an action point (created / handoff / review entry /
// reopened / commented / cancelled), or a gate decision (approve / reject).
// The frontend renders the merged, time-ordered stream as the goal's
// execution timeline — who handled it, for how long, and who holds it now.
type TimelineItem struct {
	At         string `json:"at"`                   // RFC3339 — the event's point in time
	Kind       string `json:"kind"`                 // run | action | decision
	RunID      string `json:"run_id,omitempty"`     // run: the run row (for detail fetch)
	AgentID    string `json:"agent_id,omitempty"`   // run: the executing agent
	RunStatus  string `json:"run_status,omitempty"` // run: queued|running|completed|failed|cancelled
	Attempt    int    `json:"attempt,omitempty"`    // run: machine-retry counter
	StartedAt  string `json:"started_at,omitempty"` // run: execution window
	FinishedAt string `json:"finished_at,omitempty"`
	ActorType  string `json:"actor_type,omitempty"` // action: human|agent|system
	ActorID    string `json:"actor_id,omitempty"`
	Action     string `json:"action,omitempty"` // action: created|handoff|entered_review|requested_review|parked_review|reopened|cancelled|commented|mention_cycle_failed
	Detail     string `json:"detail,omitempty"`
	GateRule   string `json:"gate_rule,omitempty"`         // decision: which rule fired
	Decision   string `json:"decision,omitempty"`          // decision: approve|reject|redirect
	Reason     string `json:"reason,omitempty"`            // decision: the human's words
	ReviewDurS int    `json:"review_duration_s,omitempty"` // decision: seconds spent in review
}

// Timeline merges the goal's runs (execution segments), activity log
// (human/system action points), and gate decisions (checkpoint verdicts)
// into one time-ordered execution flow. The current holder is derived by the
// frontend from the goal's status plus the latest non-terminal run.
func (s *GoalService) Timeline(ctx context.Context, goalID string) ([]TimelineItem, error) {
	items := []TimelineItem{}

	// 1. runs — execution segments (an agent's turn on the goal).
	rrows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, agent_id, status, attempt, queued_at, started_at, finished_at
		 FROM run WHERE goal_id=? AND goal_id<>''`, goalID)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var it TimelineItem
		var q, st, fin sql.NullString
		if err := rrows.Scan(&it.RunID, &it.AgentID, &it.RunStatus, &it.Attempt, &q, &st, &fin); err != nil {
			return nil, err
		}
		it.Kind = "run"
		// NOTE: the columns hold "" (Go zero value), not NULL — judge by
		// non-empty, never by sql.NullString.Valid (empty string is Valid).
		it.StartedAt, it.FinishedAt = st.String, fin.String
		// The segment's anchor: started_at once begun, queued_at before that.
		it.At = q.String
		if st.String != "" {
			it.At = st.String
		}
		items = append(items, it)
	}
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	// 2. activity log — human/system action points.
	arows, err := s.st.DB().QueryContext(ctx,
		`SELECT actor_type, actor_id, action, detail, created_at
		 FROM activity_log WHERE goal_id=?`, goalID)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var it TimelineItem
		var det sql.NullString
		if err := arows.Scan(&it.ActorType, &it.ActorID, &it.Action, &det, &it.At); err != nil {
			return nil, err
		}
		it.Kind = "action"
		it.Detail = det.String
		items = append(items, it)
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}

	// 3. gate decisions — human checkpoint verdicts (approve / reject).
	drows, err := s.st.DB().QueryContext(ctx,
		`SELECT gate_rule, decision, reason, decided_by, decided_at, review_duration
		 FROM gate_decision WHERE goal_id=?`, goalID)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var it TimelineItem
		var rsn sql.NullString
		if err := drows.Scan(&it.GateRule, &it.Decision, &rsn, &it.ActorType, &it.At, &it.ReviewDurS); err != nil {
			return nil, err
		}
		it.Kind = "decision"
		it.ActorID = "" // decided_by is "human"; the frontend renders the human node
		it.Reason = rsn.String
		items = append(items, it)
	}
	if err := drows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].At < items[j].At })
	return items, nil
}

func (s *GoalService) Get(ctx context.Context, id string) (*Goal, error) {
	var g Goal
	var parentID, domainID, currentAgent sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT g.id,g.title,g.description,g.parent_id,g.domain_id,g.assignee_type,g.assignee_id,g.status,g.handoff_note,g.review_request,g.human_iterations,g.created_by_type,g.created_by_id,g.created_at,g.wake_count,g.source_ref,
		        (SELECT r.agent_id FROM run r WHERE r.goal_id = g.id AND r.status IN ('running','queued') ORDER BY r.created_at DESC LIMIT 1)
		 FROM goal g WHERE g.id=?`, id).
		Scan(&g.ID, &g.Title, &g.Description, &parentID, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.WakeCount, &g.SourceRef, &currentAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.ParentID = parentID.String
	g.DomainID = domainID.String
	g.CurrentAgentID = currentAgent.String
	return &g, nil
}

// Assign changes a goal's assignee (the handoff path). Per DESIGN.md
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
	// v2: a goal handed to an agent/squad must belong to a domain — the domain
	// owns the worktree and the acceptance policy. Assigning a domain-less
	// goal to an agent would produce a run with no worktree, no verification,
	// and no deliver (the scratch-dir dead path).
	if assigneeType != "human" && g.DomainID == "" {
		return nil, NewValidationError("cannot assign to agent/squad: the goal has no domain (attach a domain first)")
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
	// The handoff note is the human's words — it belongs in the comment
	// feed (the collaboration surface), not only in the handoff_note field.
	// Written directly (not via CommentService.Create) so the mention never
	// double-triggers: the caller already enqueues the new assignee's run.
	if handoffNote != "" {
		var label string
		_ = s.st.DB().QueryRowContext(ctx,
			`SELECT name FROM agent WHERE id=?`, assigneeID).Scan(&label)
		if label == "" {
			_ = s.st.DB().QueryRowContext(ctx,
				`SELECT name FROM squad WHERE id=?`, assigneeID).Scan(&label)
		}
		if label != "" {
			content := "[@" + label + "](mention://" + assigneeType + "/" + assigneeID + ") " + handoffNote
			if _, err := s.st.DB().ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
				newID(), goalID, "human", content, now()); err != nil {
				return nil, fmt.Errorf("insert handoff comment: %w", err)
			}
		}
	}

	g.AssigneeType = assigneeType
	g.AssigneeID = assigneeID
	g.HandoffNote = handoffNote
	s.bus.Publish(ctx, events.Event{Topic: "goal:assigned", Payload: g})
	return g, nil
}

// Cancel marks a non-terminal goal cancelled. Like handoff, this does not kill
// in-flight runs: correctness comes from ReconcileOnRunEnd seeing goal.status
// cancelled and discarding the result. (Killing the process is a resource
// optimization, not a correctness requirement.)
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

// WaitChildren parks a goal while its sub-goals finish. Only active/queued
// goals may wait (terminal goals return ErrNotFound). Idempotent.
func (s *GoalService) WaitChildren(ctx context.Context, goalID string) error {
	var status string
	err := s.st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, goalID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load goal: %w", err)
	}
	if status == "blocked" {
		return nil // already waiting; idempotent
	}
	if status != "active" && status != "queued" {
		return ErrNotFound // terminal or invalid
	}
	// Wait is only meaningful with something to wait for: zero non-terminal
	// sub-goals means nothing will ever wake this goal (the wake is driven by
	// child-done) — a wait would deadlock it in blocked forever. Refuse
	// loudly: the agent sees the error and ends its turn instead of parking
	// the goal. (The dynamic wait-set covers sub-goals created BEFORE the
	// wait; nothing can create one after — the parent is blocked and mention
	// triggers only fire on active goals.)
	var inflight int
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE parent_id=? AND status NOT IN ('done','failed','cancelled')`,
		goalID).Scan(&inflight); err != nil {
		return fmt.Errorf("count children: %w", err)
	}
	if inflight == 0 {
		return NewValidationError("没有未完成的子任务可等待（wait 只用于等待子任务完成）")
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='blocked' WHERE id=?`, goalID); err != nil {
		return fmt.Errorf("wait-children: %w", err)
	}
	// Blocked = waiting on sub-goals, not working — a mention run that raced
	// ahead of the wait must not be claimed onto a waiting goal.
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
		return fmt.Errorf("cancel queued runs on wait: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'waiting_children','{}',?)`,
		newID(), goalID, "agent", "", now()); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:waiting", Payload: map[string]string{"goal_id": goalID}})
	return nil
}

// Delete removes a goal and dependents. Sub-goals are orphaned (parent_id
// cleared) rather than deleted recursively — deleting a parent must not
// silently destroy children the caller may not know about.
func (s *GoalService) Delete(ctx context.Context, goalID string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM chat_message WHERE run_id IN (SELECT id FROM run WHERE goal_id=?)`,
		`DELETE FROM gate_decision WHERE goal_id=?`,
		`DELETE FROM run WHERE goal_id=?`,
		`DELETE FROM comment WHERE goal_id=?`,
		`DELETE FROM activity_log WHERE goal_id=?`,
		`DELETE FROM schedule_run WHERE goal_id=?`,
		`UPDATE goal SET parent_id=NULL WHERE parent_id=?`,
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
// self-consistent without an external authority (DESIGN.md).
//
//	agent-assigned goal: the run's agent must equal the goal's assignee.
//	squad-assigned goal: the run's agent must be the squad's CURRENT leader
//	  — judged dynamically at reconcile time, NOT from the run's static
//	  is_leader_run mark (a leader mentioned by name via mention://agent is
//	  still the owner: authority follows the assignee relationship, not how
//	  the run was triggered). A leader change orphans the prior leader's
//	  in-flight run for free.
//	human-assigned goal: never has an agent-run owner — fall through to false.
func (s *GoalService) ownRunByGoal(ctx context.Context, tx *sql.Tx, rc goalRunContext, g Goal) (bool, error) {
	switch g.AssigneeType {
	case "agent":
		return rc.AgentID == g.AssigneeID && !rc.IsLeaderRun, nil
	case "squad":
		var leaderID string
		err := tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, g.AssigneeID).Scan(&leaderID)
		if err != nil {
			return false, fmt.Errorf("load squad leader: %w", err)
		}
		return rc.AgentID == leaderID, nil
	default:
		return false, nil // human-assigned
	}
}

// ReconcileOnRunEnd is the ONLY place that advances a goal's status based on a
// run's terminal outcome. It is called by the daemon after a run reaches a
// terminal status (completed/failed). Everything else — handoff, cancel,
// wait-children — only changes goal.status directly; they never use a run's
// result. See DESIGN.md
//
// Rules:
//
//	run.agent != current assignee  → discard (handoff/reassign orphaned run)
//	goal.status == cancelled       → discard
//	run completed, no inflight sub-goals → goal → done, wake parent
//	run completed, sub-goals inflight → leave; child-done flow owns the wake
//	run failed, attempts left → enqueue a retry run (attempt+1)
//	run failed, attempts exhausted → goal → failed
//
// insertRunResultComment lands a completed run's report in the feed as the
// agent's comment — the delivery summary is what the human rejects/approves
// against, and "review is a platform mechanism, not agent self-discipline"
// (decision 4-4): the platform writes the run's report regardless of what
// the agent said. Covers EVERY completed run — guest and assignee alike (a
// live remote run surfaced the gap: the agent had no client tools and never
// commented, so the feed showed only the human's reject with no context of
// what was rejected — "我驳回了空气"). The report is kept in FULL: it is the
// agent's words; an 800-char cut loses exactly the context a reject
// decision needs. NO dedupe: the report is the run's delivery record (a
// platform guarantee), and an agent's own voluntary comment is additional
// conversation — they do not exclude each other (an agent saying "搞定" must
// not hide the full report, nor a report hide its words).
func insertRunResultComment(ctx context.Context, tx *sql.Tx, rc goalRunContext) error {
	if rc.Status != "completed" || strings.TrimSpace(rc.Summary) == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,?,?,NULL,?,?,?)`,
		newID(), rc.GoalID, "agent", rc.AgentID, strings.TrimSpace(rc.Summary), now(), rc.RunID); err != nil {
		return fmt.Errorf("insert run-result comment: %w", err)
	}
	return nil
}

func (s *GoalService) ReconcileOnRunEnd(ctx context.Context, rc goalRunContext) error {
	// Events are collected here and published ONLY after the tx commits, so a
	// failed commit can never leave the bus with an event whose DB change
	// rolled back (DESIGN "bus.Publish after commit"). Failure-side effects
	// that need a fresh transaction (retry enqueue) are also deferred to after
	// commit.
	var pendingEvents []events.Event
	var afterCommit []func()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var g Goal
	var parentID sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id,assignee_type,assignee_id,status,parent_id FROM goal WHERE id=?`, rc.GoalID).
		Scan(&g.ID, &g.AssigneeType, &g.AssigneeID, &g.Status, &parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // goal vanished; nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("load goal for reconcile: %w", err)
	}
	g.ParentID = parentID.String

	owns, err := s.ownRunByGoal(ctx, tx, rc, g)
	if err != nil {
		return err
	}
	if !owns {
		// Orphaned run (handoff/reassign/leader-change) or a guest run
		// (mention-triggered collaboration). Its result has no authority over
		// the goal. A FAILED collaboration run is not silent though — the
		// human waiting at a checkpoint must see that the review/help run
		// failed, not an empty request (the guest-failure path: no retry, no
		// goal effect, but a durable trace in the feed).
		if rc.Status == "failed" && rc.TriggerCommentID != "" {
			summary := strings.TrimSpace(rc.Summary)
			if len(summary) > 200 {
				summary = summary[:200] + "…"
			}
			content := "协作 run 失败：" + summary
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,'system','',NULL,?,?,?)`,
				newID(), rc.GoalID, content, now(), rc.RunID); err != nil {
				return fmt.Errorf("insert guest-failure comment: %w", err)
			}
		}
		// A COMPLETED run's report lands in the feed here (orphaned/guest
		// branch). Owned runs get the same fallback in the completed case
		// below — see insertRunResultComment.
		if err := insertRunResultComment(ctx, tx, rc); err != nil {
			return err
		}
		pendingEvents = append(pendingEvents, events.Event{Topic: "run:discarded", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "orphaned",
		}})
		return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit)
	}
	if g.Status == "cancelled" {
		// Goal was cancelled while this run was in flight. Discard.
		pendingEvents = append(pendingEvents, events.Event{Topic: "run:discarded", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "cancelled",
		}})
		return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit)
	}

	switch rc.Status {
	case "completed":
		// The run's report lands in the feed as the agent's comment — the
		// human's reject/approve reads it (the reject context the feed
		// lacked. Fallback only: a self-commenting agent
		// gets no duplicate.
		if err := insertRunResultComment(ctx, tx, rc); err != nil {
			return err
		}
		// A completed run that passed machine verification has reached the
		// acceptance-judgment step (D2: inside this transaction). If the
		// goal's domain has checkpoint gates, the goal parks in review and
		// the human decides; otherwise it promotes to done as before.
		// Note: machine verification failures never reach here — the daemon
		// runs verify before finishing the run and a red verify ends the run
		// failed, flowing through the retry branch below.
		hit, reason, err := s.gatesForGoal(ctx, tx, rc)
		if err != nil {
			return fmt.Errorf("check gates: %w", err)
		}
		if hit {
			// Park in review. The handoff/wakeup note is NOT cleared — if the
			// human rejects, the next run resumes from it.
			res, err := tx.ExecContext(ctx,
				`UPDATE goal SET status='review', review_request=? WHERE id=? AND status NOT IN ('done','failed','cancelled','blocked','review')`,
				reason, rc.GoalID)
			if err != nil {
				return fmt.Errorf("park goal in review: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				// The goal is not parkable (blocked / already review /
				// terminal): the gate cannot fire here. NO activity, NO
				// reviewing event — a fake park must not mislead the review
				// trigger into firing on a non-review goal.
				break
			}
			// The activity trail must record the REAL reason that parked the
			// goal — a hardcoded {"gate":"merge"} mislabels diff_contains hits,
			// the unfrozen-policy gate, and the weak-strength default. Same
			// {"reason": ...} shape as requested_review / parked_review.
			detail, _ := json.Marshal(map[string]string{"reason": reason})
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'entered_review',?,?)`,
				newID(), rc.GoalID, "system", "", string(detail), now()); err != nil {
				return fmt.Errorf("insert review activity: %w", err)
			}
			pendingEvents = append(pendingEvents, events.Event{Topic: "goal:reviewing", Payload: map[string]any{
				"goal_id": rc.GoalID, "run_id": rc.RunID, "reason": reason,
			}})
			break
		}
		// Only promote to done if no non-terminal sub-goals remain. If
		// children are inflight, leave the goal; a child-done wake advances it.
		var inflight int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM goal WHERE parent_id=? AND status NOT IN ('done','failed','cancelled')`,
			rc.GoalID).Scan(&inflight); err != nil {
			return fmt.Errorf("count children: %w", err)
		}
		if inflight > 0 {
			break
		}
		// Clear the handoff/wakeup note as part of the same transaction that
		// promotes the goal — once this turn legitimately completes, the note
		// (a scoping instruction or child summary) is consumed. (See
		// DESIGN: the daemon no longer clears handoff_note itself; only the
		// goal layer, after confirming the run owns the goal, does.)
		if res, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='' WHERE id=? AND status NOT IN ('done','failed','cancelled','blocked','review')`,
			rc.GoalID); err != nil {
			return fmt.Errorf("promote goal done: %w", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			break // goal is blocked (waiting children) — leave for the wake
		}
		// The goal reached a terminal state — queued runs (a mention that
		// raced ahead) must not be claimed onto a finished goal.
		if _, err := tx.ExecContext(ctx,
			`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, rc.GoalID); err != nil {
			return fmt.Errorf("cancel queued runs on done: %w", err)
		}
		if g.ParentID != "" {
			if err := s.wakeParentIfReadyInTx(ctx, tx, g.ParentID, &pendingEvents); err != nil {
				return fmt.Errorf("wake parent: %w", err)
			}
		}

	case "failed":
		if rc.Attempt < maxAttempts {
			// Retry: enqueue a fresh run at attempt+1 on the same agent so
			// history is preserved. Deferred to after commit — the retry needs
			// the per-(goal,agent) coalesce, which RunService owns.
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
				`UPDATE goal SET status='failed' WHERE id=? AND status NOT IN ('done','failed','cancelled','blocked','review')`,
				rc.GoalID); err != nil {
				return fmt.Errorf("fail goal: %w", err)
			}
			// Terminal state — drop queued runs (same rule as the done path).
			if _, err := tx.ExecContext(ctx,
				`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, rc.GoalID); err != nil {
				return fmt.Errorf("cancel queued runs on fail: %w", err)
			}
		}
	}

	pendingEvents = append(pendingEvents, events.Event{Topic: "goal:finished", Payload: map[string]any{
		"goal_id": rc.GoalID, "status": rc.Status, "summary": rc.Summary,
	}})
	return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit)
}

// gatesForGoal decides whether a completed run parks the goal in review
// (DESIGN.md §5, M2 rule engine):
//
//  1. The daemon evaluated the gate rules against the run's diff and
//     recorded the fired gates on the run row (run.gates_hit) — merge always
//     fires, diff_* fire on pattern match. The goal layer reads that result:
//     the daemon computes, the goal layer judges.
//  2. Strength linkage (§5.4): a weak-verification domain with no configured
//     gates still gets a default merge gate — weak verification must not run
//     unattended.
//
// request gates are set directly by RequestApproval, not via this path.
func (s *GoalService) gatesForGoal(ctx context.Context, tx *sql.Tx, rc goalRunContext) (bool, string, error) {
	var gatesHitJSON string
	err := tx.QueryRowContext(ctx, `SELECT gates_hit FROM run WHERE id=?`, rc.RunID).Scan(&gatesHitJSON)
	if err == nil && gatesHitJSON != "" && gatesHitJSON != "[]" {
		var hit []string
		if json.Unmarshal([]byte(gatesHitJSON), &hit) == nil && len(hit) > 0 {
			return true, strings.Join(hit, "; "), nil
		}
	}
	// No gate fired for this run. Strength linkage: weak verification with no
	// gates at all still demands a human checkpoint.
	var checksJSON, strength, compiledAt string
	err = tx.QueryRowContext(ctx,
		`SELECT d.checks, d.verification_strength, d.checks_compiled_at FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, rc.GoalID).
		Scan(&checksJSON, &strength, &compiledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil // no domain → no gates
	}
	if err != nil {
		return false, "", fmt.Errorf("load domain gates: %w", err)
	}
	// The confirmation gate (决策 2-4/2-5): an UNFROZEN acceptance policy is
	// no acceptance policy — nothing was run against it (the daemon skips
	// verification for unfrozen domains), so no machine judgment exists and
	// the goal must NOT promote unattended. The human checkpoint is the
	// only safe default: "define by the human" is enforced here, not hoped
	// for.
	if compiledAt == "" {
		return true, "域验收策略未确认（checks 未冻结）——强制人工审批", nil
	}
	var checks Checks
	if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
		return false, "", fmt.Errorf("parse domain checks: %w", err)
	}
	if len(checks.Gates) > 0 {
		return false, "", nil // gates configured but none fired for this run
	}
	if strength == "weak" {
		return true, "merge (default): 弱验证域强制人工审批", nil
	}
	return false, "", nil
}

// ResolveReview handles a human checkpoint decision (DESIGN.md §4/§5):
//
//	approve  → record the gate_decision, keep the goal in review, publish
//	           goal:approved — the daemon runs the deliver step (merge +
//	           re-verify + push) and closes with MarkDelivered.
//	reject/redirect → record the gate_decision, bump human_iterations (the
//	           reject counter, SEPARATE from run.attempt), move the goal back
//	           to active with the reason as handoff_note, and enqueue a new
//	           run on the current assignee — the agent continues in the same
//	           worktree, working from the decision note.
//
// runID links the decision to the evidence run the human judged (the audit
// chain, gate_decision.run_id). The Web resolves the goal without naming a
// run (” — its review panel shows the latest completed run); the IM
// approval card carries the run id in the button value, so the decision
// lands on exactly the run whose evidence the card displayed.
func (s *GoalService) ResolveReview(ctx context.Context, goalID, runID, decision, reason string) (*Goal, error) {
	if decision != "approve" && decision != "reject" && decision != "redirect" {
		return nil, NewValidationError("decision must be approve, reject, or redirect")
	}
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if g.Status != "review" {
		return nil, NewValidationError("goal is not in review")
	}
	// Duplicate-decision guard: the deliver step runs ASYNC after an approve,
	// and the goal stays in review until it finishes — a second decision in
	// that window would race the merge. Both directions are guarded:
	//   - re-approve: the human clicking again (the page shows no feedback
	//     while deliver runs) would pile up gate_decision rows.
	//   - reject after approve: the goal would go back to active + a new run
	//     while the deliver may already have pushed — the agent would then
	//     continue on a branch whose work is already in the default branch.
	// EXCEPTION: a FAILED deliver annotates review_request ("deliver: ...")
	// and BOTH decisions must be allowed — re-approve retries the deliver,
	// reject sends the agent back to fix the conflict (the designed paths).
	deliverFailed := strings.HasPrefix(g.ReviewRequest, "deliver:")
	if decision == "approve" || decision == "reject" || decision == "redirect" {
		var lastDecision string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT decision FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, goalID).
			Scan(&lastDecision)
		if err == nil && !deliverFailed {
			if decision == "approve" && lastDecision == "approve" {
				return nil, NewValidationError("goal already approved — waiting for the deliver step (or check the deliver result)")
			}
			if (decision == "reject" || decision == "redirect") && lastDecision == "approve" {
				return nil, NewValidationError("goal is being delivered — reject is available after the deliver finishes or fails")
			}
		}
	}
	ts := now()
	// gate_rule is WHICH rule actually parked the goal — resolved from the
	// evidence run's gates_hit (the daemon records the fired gate names
	// there), NOT hardcoded: the health-learning aggregation (GateStats)
	// groups by rule, and a diff_contains decision recorded as "merge" would
	// corrupt the learning data.
	rule, err := s.resolveGateRule(ctx, goalID, runID, g.ReviewRequest)
	if err != nil {
		return nil, err
	}
	// review_duration: seconds spent in review before this decision — the
	// health-learning data source (gate_decision.review_duration). Measured
	// from the most recent entry into review (any of the three park paths).
	duration := 0
	var enteredAt string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT created_at FROM activity_log WHERE goal_id=? AND action IN ('entered_review','requested_review','parked_review') ORDER BY created_at DESC LIMIT 1`,
		goalID).Scan(&enteredAt); err == nil && enteredAt != "" {
		if et, err := time.Parse(time.RFC3339Nano, enteredAt); err == nil {
			if dt, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if secs := int(dt.Sub(et).Seconds()); secs > 0 {
					duration = secs
				}
			}
		}
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO gate_decision (id,goal_id,run_id,gate_rule,decision,reason,decided_by,decided_at,review_duration) VALUES (?,?,?,?,?,?,?,?,?)`,
		newID(), goalID, runID, rule, decision, reason, "human", ts, duration); err != nil {
		return nil, fmt.Errorf("insert gate_decision: %w", err)
	}
	// The decision's reason is the human's words — the comment feed is where
	// the agent's next run reads recent human comments, so the reject reason
	// must be there (not only in gate_decision / handoff_note).
	if reason != "" {
		verb := map[string]string{"approve": "批准", "reject": "驳回", "redirect": "改判"}[decision]
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
			newID(), goalID, "human", verb+"："+reason, ts); err != nil {
			return nil, fmt.Errorf("insert decision comment: %w", err)
		}
	}

	switch decision {
	case "approve":
		// Stay in review; the daemon's deliver step is the only mover from
		// here (MarkDelivered closes it).
		s.bus.Publish(ctx, events.Event{Topic: "goal:approved", Payload: map[string]any{
			"goal_id": goalID, "reason": reason,
		}})
	case "reject", "redirect":
		note := "Review decision: " + decision
		if reason != "" {
			note += "\n" + reason
		}
		res, err := s.st.DB().ExecContext(ctx,
			`UPDATE goal SET status='active', handoff_note=?, human_iterations=human_iterations+1, review_request='' WHERE id=? AND status='review'`,
			note, goalID)
		if err != nil {
			return nil, fmt.Errorf("reject review: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, NewValidationError("goal is no longer in review")
		}
		if s.runSvc == nil {
			return nil, errors.New("goalSvc.runSvc not wired")
		}
		// Continue on the current assignee. attempt resets to 1: a reject
		// iteration is a fresh human-directed cycle, not a machine retry —
		// the reject count lives in goal.human_iterations (DESIGN.md §4).
		agentID, isLeader, squadID, err := s.runSvc.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
		if err != nil {
			return nil, err
		}
		if agentID != "" {
			if err := s.runSvc.EnqueueExisting(ctx, goalID, agentID, 1, isLeader, squadID); err != nil {
				return nil, fmt.Errorf("enqueue after reject: %w", err)
			}
		}
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:review_resolved", Payload: map[string]any{
		"goal_id": goalID, "decision": decision,
	}})
	return s.Get(ctx, goalID)
}

// RequestApproval is the behavior gate (DESIGN.md §5, M2): an agent that
// hits a decision point it must not make alone parks the goal in review and
// asks the human — via `agentwork-cli goal request-approval --reason "..."`.
// The in-flight run keeps running; when it reports in, the reconcile sees
// goal.status=review and does NOT advance it (review is exclusive — both the
// done and failed promotions exclude it). The human's approve/reject then
// resolves the goal like any other checkpoint.
func (s *GoalService) RequestApproval(ctx context.Context, goalID, reason string) (*Goal, error) {
	if reason == "" {
		return nil, NewValidationError("reason is required")
	}
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request=? WHERE id=? AND status='active'`,
		"agent 请求审批: "+reason, goalID)
	if err != nil {
		return nil, fmt.Errorf("request approval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, NewValidationError("goal is not active — cannot request approval")
	}
	detail, _ := json.Marshal(map[string]string{"reason": reason})
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'requested_review',?,?)`,
		newID(), goalID, "agent", "", string(detail), now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:reviewing", Payload: map[string]any{
		"goal_id": goalID, "reason": "agent 请求审批: " + reason,
	}})
	return s.Get(ctx, goalID)
}

// IssueSource returns the goal's external source reference and the owning
// domain's git_credentials (M4-B: the issue identity + the GitHub token the
// platform uses to act on it). ok=false when the goal is not issue-sourced.
func (s *GoalService) IssueSource(ctx context.Context, goalID string) (ref, token string, ok bool, err error) {
	err = s.st.DB().QueryRowContext(ctx,
		`SELECT g.source_ref, d.git_credentials FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, goalID).
		Scan(&ref, &token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return ref, token, ref != "", nil
}

// Reopen restarts a terminal goal — failed/cancelled (the failed-goal human
// take-over path, DESIGN.md §13) AND done (the comment-triggered reopen:
// a mention on a done goal is "this task is not over" — an追加需求, GitHub's
// reopen-and-comment): back to active with the reason as handoff_note, and a
// fresh run on the current assignee (attempt resets — a reopen is a new
// human-directed cycle, like a reject).
func (s *GoalService) Reopen(ctx context.Context, goalID, reason string) (*Goal, error) {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if g.Status != "failed" && g.Status != "cancelled" && g.Status != "done" {
		return nil, NewValidationError("only done, failed, or cancelled goals can be reopened")
	}
	note := "Reopened"
	if reason != "" {
		note += ": " + reason
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='active', handoff_note=?, review_request='' WHERE id=? AND status IN ('done','failed','cancelled')`,
		note, goalID); err != nil {
		return nil, fmt.Errorf("reopen goal: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'reopened',?,?)`,
		newID(), goalID, "human", "", "{}", now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	// The reopen reason is the human's words — land it in the comment feed
	// so the conversation stays complete (and the next run's prompt injects
	// it via the recent-human-comments mechanism).
	if reason != "" {
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
			newID(), goalID, "human", "重开："+reason, now()); err != nil {
			return nil, fmt.Errorf("insert reopen comment: %w", err)
		}
	}
	// Fresh run on the current assignee (human-assigned goals stay manual).
	if g.AssigneeType == "agent" || g.AssigneeType == "squad" {
		agentID, isLeader, squadID, err := s.runSvc.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
		if err != nil {
			return nil, err
		}
		if agentID != "" {
			if err := s.runSvc.EnqueueExisting(ctx, goalID, agentID, 1, isLeader, squadID); err != nil {
				return nil, fmt.Errorf("enqueue after reopen: %w", err)
			}
		}
	}
	return s.Get(ctx, goalID)
}

// ParkForManualReview parks an active goal in review because the platform
// found something only a human can resolve — e.g. unattributed worktree
// changes at run start (DESIGN.md §4: a run must not start on a
// worktree carrying changes nobody can account for). The human's
// approve/reject then flows through the normal review path.
func (s *GoalService) ParkForManualReview(ctx context.Context, goalID, reason string) error {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request=? WHERE id=? AND status='active'`,
		reason, goalID)
	if err != nil {
		return fmt.Errorf("park goal for manual review: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return NewValidationError("goal is not active — cannot park for manual review")
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'parked_review',?,?)`,
		newID(), goalID, "system", "", `{"reason":"worktree-unattributed"}`, now()); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:reviewing", Payload: map[string]any{
		"goal_id": goalID, "reason": reason,
	}})
	return nil
}

// resolveGateRule names the rule that actually parked the goal in review:
//   - the named run's gates_hit[0] (the IM card carries the evidence run),
//   - else the goal's latest completed run's gates_hit[0] (the Web panel),
//   - else the review_request source (a behavior-gate request is "request"),
//   - else "merge" (the strength-linkage default gate fires with empty
//     gates_hit).
func (s *GoalService) resolveGateRule(ctx context.Context, goalID, runID, reviewRequest string) (string, error) {
	if runID == "" {
		// The Web panel resolves without naming a run — use the latest
		// completed run, whose gates_hit the daemon recorded.
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT id FROM run WHERE goal_id=? AND status='completed'
			 ORDER BY finished_at DESC LIMIT 1`, goalID).Scan(&runID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("find evidence run: %w", err)
		}
	}
	if runID != "" {
		var hitJSON string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT gates_hit FROM run WHERE id=?`, runID).Scan(&hitJSON)
		if err == nil && hitJSON != "" && hitJSON != "[]" {
			var hit []string
			if json.Unmarshal([]byte(hitJSON), &hit) == nil && len(hit) > 0 {
				if name, _, ok := strings.Cut(hit[0], ":"); ok && strings.TrimSpace(name) != "" {
					return strings.TrimSpace(name), nil
				}
				return hit[0], nil
			}
		}
	}
	if strings.HasPrefix(reviewRequest, "agent 请求审批") {
		return "request", nil
	}
	return "merge", nil
}

// GateStat is one gate rule's decision history — the health-learning data
// source (DESIGN.md §13): a gate approved every time is a candidate for
// removal; one rejected repeatedly is a candidate for tightening.
type GateStat struct {
	Rule     string `json:"rule"` // gate_rule as recorded (merge, diff_contains, ...)
	Total    int    `json:"total"`
	Approved int    `json:"approved"`
	Rejected int    `json:"rejected"`
}

// GateStats aggregates gate_decision by rule. Sorted by total descending so
// the busiest gates surface first.
func (s *GoalService) GateStats(ctx context.Context) ([]GateStat, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT gate_rule,
		        COUNT(*) AS total,
		        SUM(CASE WHEN decision='approve' THEN 1 ELSE 0 END) AS approved,
		        SUM(CASE WHEN decision IN ('reject','redirect') THEN 1 ELSE 0 END) AS rejected
		 FROM gate_decision
		 GROUP BY gate_rule
		 ORDER BY total DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GateStat{}
	for rows.Next() {
		var g GateStat
		if err := rows.Scan(&g.Rule, &g.Total, &g.Approved, &g.Rejected); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MarkDelivered closes the deliver step (DESIGN.md §7), called by the
// daemon after its deterministic merge + re-verify + push:
//
//	success → review → done (handoff_note cleared, parent woken)
//	failure → stays in review with the reason annotated (review_request),
//	          so the human can retry deliver or reject the change back.
//
// fixCommits ("<full sha> <title>") is the fix evidence the daemon collected;
// the goal layer passes it through verbatim to the delivered event — the
// issue closer links it. The goal layer never parses it (not its domain).
func (s *GoalService) MarkDelivered(ctx context.Context, goalID string, success bool, note string, fixCommits []string) (*Goal, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var parentID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT parent_id FROM goal WHERE id=?`, goalID).Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load goal: %w", err)
	}

	var deliverEvents []events.Event
	event := "goal:deliver_failed"
	if success {
		// M0 simplification: a delivered goal is done regardless of children
		// (the deliver already merged this goal's branch). Parent coordination
		// semantics are refined in M1/M2.
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='' WHERE id=? AND status='review'`, goalID); err != nil {
			return nil, fmt.Errorf("mark delivered: %w", err)
		}
		// Terminal state — drop queued runs (same rule as the reconcile paths).
		if _, err := tx.ExecContext(ctx,
			`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
			return nil, fmt.Errorf("cancel queued runs on delivered: %w", err)
		}
		event = "goal:delivered"
		if parentID.Valid && parentID.String != "" {
			if err := s.wakeParentIfReadyInTx(ctx, tx, parentID.String, &deliverEvents); err != nil {
				return nil, fmt.Errorf("wake parent: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET review_request=? WHERE id=? AND status='review'`, note, goalID); err != nil {
			return nil, fmt.Errorf("annotate deliver failure: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.bus.Publish(ctx, events.Event{Topic: event, Payload: map[string]any{
		"goal_id": goalID, "note": note, "commits": fixCommits,
	}})
	// The wake's run events, published after commit (invariant 13).
	for _, e := range deliverEvents {
		s.bus.Publish(ctx, e)
	}
	return s.Get(ctx, goalID)
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

// wakeParentIfReadyInTx is the child→parent notification. When a sub-goal
// reaches a terminal status, if the parent is blocked (waiting on children)
// and all its non-terminal sub-goals are now terminal, re-queue a run on the
// parent's current assignee with a wakeup note summarising the children.
// Guarded by `WHERE status='blocked'` so a concurrent wake bails (double-wake
// prevention). Per DESIGN.md (dynamic wait set).
// wakeParentIfReadyInTx wakes a blocked parent once all its children are
// terminal. evs collects the run events produced by the wake's enqueue —
// the caller publishes them after its own commit (invariant 13).
func (s *GoalService) wakeParentIfReadyInTx(ctx context.Context, tx *sql.Tx, parentID string, evs *[]events.Event) error {
	var parentStatus, assigneeType, assigneeID string
	var wakeCount int
	err := tx.QueryRowContext(ctx,
		`SELECT status, assignee_type, assignee_id, wake_count FROM goal WHERE id=?`, parentID).
		Scan(&parentStatus, &assigneeType, &assigneeID, &wakeCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if parentStatus != "blocked" {
		return nil
	}
	if assigneeType != "agent" && assigneeType != "squad" {
		return nil // human-assigned parents stay for manual review
	}
	var inflight int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE parent_id=? AND status NOT IN ('done','failed','cancelled')`,
		parentID).Scan(&inflight); err != nil {
		return err
	}
	if inflight > 0 {
		return nil
	}
	// Runaway guard (P0-1): a parent would cycle forever if each woken turn
	// created fresh sub-goals + waited again. Cap wakeups at maxWakeCycles;
	// beyond that, refuse to wake and force the goal failed so the loop
	// surfaces to a human instead of burning runs. (Per DESIGN §9: the loop
	// guard is a state-machine invariant, not a prompt pleasantry.)
	if wakeCount >= maxWakeCycles {
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='failed' WHERE id=? AND status NOT IN ('done','failed','cancelled')`,
			parentID); err != nil {
			return fmt.Errorf("force-fail runaway parent: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'runaway_wake_limit','{}',?)`,
			newID(), parentID, "system", "", now()); err != nil {
			return fmt.Errorf("insert runaway activity: %w", err)
		}
		return nil
	}
	note, err := s.buildWakeupNoteInTx(ctx, tx, parentID)
	if err != nil {
		return err
	}
	if res, err := tx.ExecContext(ctx,
		`UPDATE goal SET status='active', handoff_note=?, wake_count=wake_count+1 WHERE id=? AND status='blocked'`,
		note, parentID); err != nil {
		return fmt.Errorf("wake parent: %w", err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		return nil // someone else woke it
	}
	// Enqueue the parent's run in THIS tx (blocked→active + enqueue must be
	// one atomic operation, or two parallel child-dones could double-enqueue
	// the parent). The coalesce check inside EnqueueExistingTx sees this tx's
	// un-committed state. The returned run event is collected for the caller
	// to publish after its commit (invariant 13).
	if assigneeType == "agent" {
		_, ev, err := s.runSvc.EnqueueExistingTx(ctx, tx, parentID, assigneeID, 1, false, "")
		if err != nil {
			return fmt.Errorf("enqueue woke parent: %w", err)
		}
		if ev != nil {
			*evs = append(*evs, *ev)
		}
	} else { // squad: woken run is a leader run on the squad's current leader
		var leaderID string
		if err := tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leaderID); err != nil {
			return fmt.Errorf("load squad leader for wake: %w", err)
		}
		_, ev, err := s.runSvc.EnqueueExistingTx(ctx, tx, parentID, leaderID, 1, true, assigneeID)
		if err != nil {
			return fmt.Errorf("enqueue woke leader: %w", err)
		}
		if ev != nil {
			*evs = append(*evs, *ev)
		}
	}
	return nil
}

// NotifyChildDone is the child→parent trigger: after a sub-goal reaches a
// terminal status, see whether the parent is blocked (waiting on children)
// and all its sub-goals are now terminal, and if so re-queue the parent's
// current assignee with a wakeup note. Idempotent via the `WHERE status='blocked'`
// guard inside wakeParentIfReadyInTx (a concurrent child-done bails).
func (s *GoalService) NotifyChildDone(ctx context.Context, childGoalID string) error {
	var parentID sql.NullString
	err := s.st.DB().QueryRowContext(ctx, `SELECT parent_id FROM goal WHERE id=?`, childGoalID).Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load parent_id: %w", err)
	}
	if !parentID.Valid || parentID.String == "" {
		return nil
	}
	// The authoritative guard runs inside the transaction in
	// wakeParentIfReadyInTx.
	var notifyEvents []events.Event
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.wakeParentIfReadyInTx(ctx, tx, parentID.String, &notifyEvents); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Publish the wake's run events only after commit (invariant 13).
	for _, e := range notifyEvents {
		s.bus.Publish(ctx, e)
	}
	return nil
}

// WakeupParentIfReady is a public alias preserved for callers that name it the
// multica way.
func (s *GoalService) WakeupParentIfReady(ctx context.Context, childGoalID string) error {
	return s.NotifyChildDone(ctx, childGoalID)
}

func (s *GoalService) buildWakeupNoteInTx(ctx context.Context, tx *sql.Tx, parentID string) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, title, status, COALESCE(
		   (SELECT result_summary FROM run WHERE goal_id=goal.id AND status IN ('completed','failed') ORDER BY finished_at DESC LIMIT 1), '')
		 FROM goal WHERE parent_id=? ORDER BY created_at`, parentID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Sub-goals complete:\n")
	for rows.Next() {
		var id, title, status, summary string
		if err := rows.Scan(&id, &title, &status, &summary); err != nil {
			rows.Close()
			return "", err
		}
		fmt.Fprintf(&b, "- %s [%s]", title, status)
		if summary != "" {
			b.WriteString(": ")
			b.WriteString(summary)
		}
		b.WriteByte('\n')
	}
	rows.Close()
	return b.String(), rows.Err()
}

// assigneeLabel resolves an assignee's display name for the creation comment
// (agent name / squad name).
func (s *GoalService) assigneeLabel(ctx context.Context, tx *sql.Tx, atype, aid string) (string, error) {
	var name string
	var err error
	switch atype {
	case "agent":
		err = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, aid).Scan(&name)
	case "squad":
		err = tx.QueryRowContext(ctx, `SELECT name FROM squad WHERE id=?`, aid).Scan(&name)
	default:
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return name, nil
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
