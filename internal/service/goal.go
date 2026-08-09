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

// Goal is a work item (the product plane). It is the SOLE holder of state
// authority: any change to its status flows through ReconcileOnRunEnd, which
// checks whether the reporting run still belongs to the current assignee
// before touching status. See DESIGN.zh.md §2/§7.
type Goal struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	ParentID        string `json:"parent_id"`
	DomainID        string `json:"domain_id"` // owning domain (required for agent/squad goals — v2)
	AssigneeType    string `json:"assignee_type"` // agent | squad | human
	AssigneeID      string `json:"assignee_id"`
	Status          string `json:"status"` // backlog|active|done|failed|blocked|review|cancelled
	HandoffNote     string `json:"handoff_note"`
	ReviewRequest   string `json:"review_request"` // gate trigger reason / deliver-failure note
	HumanIterations int    `json:"human_iterations"` // reject iterations (separate from run.attempt)
	CreatedByType   string `json:"created_by_type"` // human | agent
	CreatedByID     string `json:"created_by_id"`
	CreatedAt       string `json:"created_at"`
	WakeCount       int    `json:"wake_count"` // bumped each blocked→active wakeup; bounded to break runaway re-fan-out
}

// goalRunContext is what ReconcileOnRunEnd reasons about. Carried separately
// so the reconciliation logic is testable without a live run row.
type goalRunContext struct {
	RunID        string
	GoalID       string
	AgentID      string
	IsLeaderRun  bool
	SquadID      string
	Status       string // run's terminal status: completed|failed
	Attempt      int
	Summary      string
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
	// the worktree and the acceptance policy (DESIGN.v2.md §2). Human/backlog
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
		`INSERT INTO goal (id,title,description,parent_id,domain_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Title, g.Description, parentID, domainID, g.AssigneeType, g.AssigneeID, g.Status, g.HandoffNote, g.CreatedByType, g.CreatedByID, g.CreatedAt); err != nil {
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
		`SELECT id,title,description,parent_id,domain_id,assignee_type,assignee_id,status,handoff_note,review_request,human_iterations,created_by_type,created_by_id,created_at,wake_count
		 FROM goal ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		var g Goal
		var parentID, domainID sql.NullString
		if err := rows.Scan(&g.ID, &g.Title, &g.Description, &parentID, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.WakeCount); err != nil {
			return nil, err
		}
		g.ParentID = parentID.String
		g.DomainID = domainID.String
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *GoalService) Get(ctx context.Context, id string) (*Goal, error) {
	var g Goal
	var parentID, domainID sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,title,description,parent_id,domain_id,assignee_type,assignee_id,status,handoff_note,review_request,human_iterations,created_by_type,created_by_id,created_at,wake_count
		 FROM goal WHERE id=?`, id).
		Scan(&g.ID, &g.Title, &g.Description, &parentID, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.WakeCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.ParentID = parentID.String
	g.DomainID = domainID.String
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
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='blocked' WHERE id=?`, goalID); err != nil {
		return fmt.Errorf("wait-children: %w", err)
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
// self-consistent without an external authority (DESIGN.zh.md §7).
//
//	agent-assigned goal: the run's agent must equal the goal's assignee.
//	squad-assigned goal: the run must be a leader run whose agent is that
//	  squad's current leader. (A leader change, if ever supported, orphaning
//	  the prior leader's in-flight run is handled here for free.)
//	human-assigned goal: never has an agent-run owner — fall through to false.
func (s *GoalService) ownRunByGoal(ctx context.Context, tx *sql.Tx, rc goalRunContext, g Goal) (bool, error) {
	switch g.AssigneeType {
	case "agent":
		return rc.AgentID == g.AssigneeID && !rc.IsLeaderRun, nil
	case "squad":
		if !rc.IsLeaderRun || rc.SquadID != g.AssigneeID {
			return false, nil
		}
		var leaderID string
		err := tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, rc.SquadID).Scan(&leaderID)
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
// result. See DESIGN.zh.md §7.
//
// Rules:
//
//	run.agent != current assignee  → discard (handoff/reassign orphaned run)
//	goal.status == cancelled       → discard
//	run completed, no inflight sub-goals → goal → done, wake parent
//	run completed, sub-goals inflight → leave; child-done flow owns the wake
//	run failed, attempts left → enqueue a retry run (attempt+1)
//	run failed, attempts exhausted → goal → failed
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
		// Orphaned run (handoff/reassign/leader-change). Its result has no
		// authority over the goal. Drop it silently.
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
		// A completed run that passed machine verification has reached the
		// acceptance-judgment step (D2: inside this transaction). If the
		// goal's domain has checkpoint gates, the goal parks in review and
		// the human decides; otherwise it promotes to done as before.
		// Note: machine verification failures never reach here — the daemon
		// runs verify before finishing the run and a red verify ends the run
		// failed, flowing through the retry branch below.
		hit, reason, err := s.gatesForGoal(ctx, tx, rc.GoalID)
		if err != nil {
			return fmt.Errorf("check gates: %w", err)
		}
		if hit {
			// Park in review. The handoff/wakeup note is NOT cleared — if the
			// human rejects, the next run resumes from it.
			if _, err := tx.ExecContext(ctx,
				`UPDATE goal SET status='review', review_request=? WHERE id=? AND status NOT IN ('done','failed','cancelled','blocked','review')`,
				reason, rc.GoalID); err != nil {
				return fmt.Errorf("park goal in review: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'entered_review',?,?)`,
				newID(), rc.GoalID, "system", "", `{"gate":"merge"}`, now()); err != nil {
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
			`UPDATE goal SET status='done', handoff_note='' WHERE id=? AND status NOT IN ('done','failed','cancelled','blocked')`,
			rc.GoalID); err != nil {
			return fmt.Errorf("promote goal done: %w", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			break // goal is blocked (waiting children) — leave for the wake
		}
		if g.ParentID != "" {
			if err := s.wakeParentIfReadyInTx(ctx, tx, g.ParentID); err != nil {
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
				`UPDATE goal SET status='failed' WHERE id=? AND status NOT IN ('done','failed','cancelled','blocked')`,
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

// gatesForGoal reads the goal's domain gates (DESIGN.v2.md §5). M0 ships a
// single gate ("merge"): any domain with gates parks completed goals in
// review until the human decides. Goals without a domain have no gates.
func (s *GoalService) gatesForGoal(ctx context.Context, tx *sql.Tx, goalID string) (bool, string, error) {
	var domainID, checksJSON string
	err := tx.QueryRowContext(ctx,
		`SELECT d.id, d.checks FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, goalID).
		Scan(&domainID, &checksJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil // no domain → no gates
	}
	if err != nil {
		return false, "", fmt.Errorf("load domain gates: %w", err)
	}
	var checks Checks
	if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
		return false, "", fmt.Errorf("parse domain checks: %w", err)
	}
	if len(checks.Gates) == 0 {
		return false, "", nil
	}
	var parts []string
	for _, g := range checks.Gates {
		parts = append(parts, g.Name+": "+g.When)
	}
	return true, strings.Join(parts, "; "), nil
}

// ResolveReview handles a human checkpoint decision (DESIGN.v2.md §4/§5):
//   approve  → record the gate_decision, keep the goal in review, publish
//              goal:approved — the daemon runs the deliver step (merge +
//              re-verify + push) and closes with MarkDelivered.
//   reject/redirect → record the gate_decision, bump human_iterations (the
//              reject counter, SEPARATE from run.attempt), move the goal back
//              to active with the reason as handoff_note, and enqueue a new
//              run on the current assignee — the agent continues in the same
//              worktree, working from the decision note.
func (s *GoalService) ResolveReview(ctx context.Context, goalID, decision, reason string) (*Goal, error) {
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
	ts := now()
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO gate_decision (id,goal_id,run_id,gate_rule,decision,reason,decided_by,decided_at,review_duration) VALUES (?,?,?,?,?,?,?,?,?)`,
		newID(), goalID, "", "merge", decision, reason, "human", ts, 0); err != nil {
		return nil, fmt.Errorf("insert gate_decision: %w", err)
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
		// the reject count lives in goal.human_iterations (DESIGN.v2.md §4).
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

// MarkDelivered closes the deliver step (DESIGN.v2.md §7), called by the
// daemon after its deterministic merge + re-verify + push:
//   success → review → done (handoff_note cleared, parent woken)
//   failure → stays in review with the reason annotated (review_request),
//             so the human can retry deliver or reject the change back.
func (s *GoalService) MarkDelivered(ctx context.Context, goalID string, success bool, note string) (*Goal, error) {
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

	event := "goal:deliver_failed"
	if success {
		// M0 simplification: a delivered goal is done regardless of children
		// (the deliver already merged this goal's branch). Parent coordination
		// semantics are refined in M1/M2.
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='' WHERE id=? AND status='review'`, goalID); err != nil {
			return nil, fmt.Errorf("mark delivered: %w", err)
		}
		event = "goal:delivered"
		if parentID.Valid && parentID.String != "" {
			if err := s.wakeParentIfReadyInTx(ctx, tx, parentID.String); err != nil {
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
		"goal_id": goalID, "note": note,
	}})
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
// prevention). Per DESIGN.zh.md §5.2 (dynamic wait set).
func (s *GoalService) wakeParentIfReadyInTx(ctx context.Context, tx *sql.Tx, parentID string) error {
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
	// un-committed state.
	if assigneeType == "agent" {
		if _, err := s.runSvc.EnqueueExistingTx(ctx, tx, parentID, assigneeID, 1, false, ""); err != nil {
			return fmt.Errorf("enqueue woke parent: %w", err)
		}
	} else { // squad: woken run is a leader run on the squad's current leader
		var leaderID string
		if err := tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leaderID); err != nil {
			return fmt.Errorf("load squad leader for wake: %w", err)
		}
		if _, err := s.runSvc.EnqueueExistingTx(ctx, tx, parentID, leaderID, 1, true, assigneeID); err != nil {
			return fmt.Errorf("enqueue woke leader: %w", err)
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
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.wakeParentIfReadyInTx(ctx, tx, parentID.String); err != nil {
		return err
	}
	return tx.Commit()
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