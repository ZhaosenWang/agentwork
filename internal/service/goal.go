package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Goal is a work item (the product plane). It is the SOLE holder of state
// authority: any change to its status flows through ReconcileOnRunEnd, which
// checks whether the reporting run still belongs to the current assignee
// before touching status. See DESIGN.md §9.
type Goal struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	DomainID        string `json:"domain_id"`     // owning domain (required for agent/squad goals — v2)
	AssigneeType    string `json:"assignee_type"` // agent | squad | human
	AssigneeID      string `json:"assignee_id"`
	Status          string `json:"status"` // backlog|active|done|failed|review|cancelled
	HandoffNote     string `json:"handoff_note"`
	ReviewRequest   string `json:"review_request"`   // gate trigger reason / deliver-failure note
	HumanIterations int    `json:"human_iterations"` // reject iterations (separate from run.attempt)
	CreatedByType   string `json:"created_by_type"`  // human | agent
	CreatedByID     string `json:"created_by_id"`
	CreatedAt       string `json:"created_at"`
	SourceRef       string `json:"source_ref"` // external source (M4-B): "github:owner/repo#123"
	// Attention is the v2 derived OwnerAttention persisted by ReconcileGoal
	// (决策 6-8): '' | integration | recovery | user_action (comma-joined).
	Attention string `json:"attention"`
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
	Status           string // run's terminal status: completed|failed|cancelled
	Attempt          int
	Summary          string
	TriggerCommentID string // mention/协作来源（guest run 失败留痕用）
	// Sub-goal runs (决策 6-1/6-9): the sub-goal this run executes and the
	// Change revision refs the daemon stamped at run end.
	Role      string
	SubGoalID string
	BaseRef   string
	HeadRef   string
}

const maxAttempts = 3

// Handoff-cycle guard thresholds (决策 5-7): ≥4 ownership transitions write a
// system warning comment; ≥8 park the goal in review for a human decision —
// a collaboration anomaly is not a task failure (unlike the mention pingpong
// guard, 决策 4-2).
const (
	handoffWarnThreshold  = 4
	handoffCycleThreshold = 8
)

type GoalService struct {
	st     *store.Store
	bus    *events.Bus
	runSvc *RunService // back-reference for retry/wake enqueue (same package)

	// reconcileLocks serializes ReconcileGoal per goal (single-process): an
	// event storm fires run.terminal + sub_goal.* concurrently, and racing
	// write transactions surface as SQLITE_BUSY under load.
	reconcileMu    sync.Mutex
	reconcileLocks map[string]*sync.Mutex
}

func NewGoalService(st *store.Store, bus *events.Bus) *GoalService {
	return &GoalService{st: st, bus: bus, reconcileLocks: make(map[string]*sync.Mutex)}
}

// lockReconcile serializes one goal's reconciles (cheap map entry per goal).
func (s *GoalService) lockReconcile(goalID string) func() {
	s.reconcileMu.Lock()
	m, ok := s.reconcileLocks[goalID]
	if !ok {
		m = &sync.Mutex{}
		s.reconcileLocks[goalID] = m
	}
	s.reconcileMu.Unlock()
	m.Lock()
	return func() {
		m.Unlock()
		s.reconcileMu.Lock()
		delete(s.reconcileLocks, goalID)
		s.reconcileMu.Unlock()
	}
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
	// Note: review is a transitive status (the checkpoint gate) — reject it at
	// creation so callers can't plant a goal in a waiting state. (blocked was
	// retired with the wait-children model, 决策 6-10; the check stays as a
	// belt-and-braces guard against stale clients.)
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

	g.ID = newID()
	g.CreatedAt = now()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var domainID any
	if g.DomainID != "" {
		domainID = g.DomainID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,domain_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at,source_ref)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Title, g.Description, domainID, g.AssigneeType, g.AssigneeID, g.Status, g.HandoffNote, g.CreatedByType, g.CreatedByID, g.CreatedAt, g.SourceRef); err != nil {
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
		`SELECT g.id,g.title,g.description,g.domain_id,g.assignee_type,g.assignee_id,g.status,g.handoff_note,g.review_request,g.human_iterations,g.created_by_type,g.created_by_id,g.created_at,g.source_ref,g.attention,
		        (SELECT r.agent_id FROM run r WHERE r.goal_id = g.id AND r.status IN ('running','queued') ORDER BY r.created_at DESC LIMIT 1)
		 FROM goal g ORDER BY g.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Goal{}
	for rows.Next() {
		var g Goal
		var domainID, currentAgent sql.NullString
		if err := rows.Scan(&g.ID, &g.Title, &g.Description, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.SourceRef, &g.Attention, &currentAgent); err != nil {
			return nil, err
		}
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
	Role       string `json:"role,omitempty"`       // run: owner|subgoal|consult|review|verify
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
		`SELECT id, agent_id, status, attempt, queued_at, started_at, finished_at, role
		 FROM run WHERE goal_id=? AND goal_id<>''`, goalID)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var it TimelineItem
		var q, st, fin sql.NullString
		if err := rrows.Scan(&it.RunID, &it.AgentID, &it.RunStatus, &it.Attempt, &q, &st, &fin, &it.Role); err != nil {
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

	// 2. activity log — human/system action points. Handoffs are excluded:
	// they render from the richer handoff_event rows (step 4, 决策 5-9).
	arows, err := s.st.DB().QueryContext(ctx,
		`SELECT actor_type, actor_id, action, detail, created_at
		 FROM activity_log WHERE goal_id=? AND action != 'handoff'`, goalID)
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

	// 3. handoff events — ownership transitions (决策 5-9, the rich audit row:
	// from/to + reason + both run links). Rendered as action points.
	hrows, err := s.st.DB().QueryContext(ctx,
		`SELECT from_type, from_id, to_type, to_id, from_run_id, to_run_id, reason, actor_type, actor_id, created_at
		 FROM handoff_event WHERE goal_id=?`, goalID)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var it TimelineItem
		var ft, fi, tt, ti, fr, tr, rsn sql.NullString
		if err := hrows.Scan(&ft, &fi, &tt, &ti, &fr, &tr, &rsn, &it.ActorType, &it.ActorID, &it.At); err != nil {
			return nil, err
		}
		it.Kind = "action"
		it.Action = "handoff"
		det, _ := json.Marshal(map[string]string{
			"from":        ft.String + "/" + fi.String,
			"to":          tt.String + "/" + ti.String,
			"reason":      rsn.String,
			"from_run_id": fr.String,
			"to_run_id":   tr.String,
		})
		it.Detail = string(det)
		items = append(items, it)
	}
	if err := hrows.Err(); err != nil {
		return nil, err
	}

	// 4. gate decisions — human checkpoint verdicts (approve / reject).
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
	var domainID, currentAgent sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT g.id,g.title,g.description,g.domain_id,g.assignee_type,g.assignee_id,g.status,g.handoff_note,g.review_request,g.human_iterations,g.created_by_type,g.created_by_id,g.created_at,g.source_ref,g.attention,
		        (SELECT r.agent_id FROM run r WHERE r.goal_id = g.id AND r.status IN ('running','queued') ORDER BY r.created_at DESC LIMIT 1)
		 FROM goal g WHERE g.id=?`, id).
		Scan(&g.ID, &g.Title, &g.Description, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.SourceRef, &g.Attention, &currentAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.DomainID = domainID.String
	g.CurrentAgentID = currentAgent.String
	return &g, nil
}

// Assign changes a goal's assignee (the handoff path). The SERVICE layer does
// NOT cancel in-flight runs — correctness comes from ReconcileOnRunEnd: an
// orphaned run reporting in later sees run.agent != goal.assignee and its
// result is discarded without touching goal.status. The DAEMON layer does the
// resource-level cut (决策 4-9): on the goal:assigned event it terminates the
// old owner's running run via the runCancels registry, so the new owner's
// queued run is not deadlocked behind per-goal serialization. The new
// assignee's run is enqueued by the caller (RunService).
//
// actorType/actorID name WHO performs the handoff (决策 5-6, Invariant 2):
// "agent" enforces that only the goal's CURRENT owner may hand it off
// (agent-assigned → the assignee; squad-assigned → the squad's current
// leader; human-assigned → no agent may). The HTTP handler passes "" (human)
// — single-user, the HTTP surface is the human's; the MCP handoff_goal tool
// passes the calling run's agent.
func (s *GoalService) Assign(ctx context.Context, goalID, assigneeType, assigneeID, handoffNote, actorType, actorID string) (*Goal, error) {
	if actorType == "" {
		actorType = "human"
	}
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

	// Handoff permission (决策 5-6): an agent actor must be the goal's current
	// owner. Human actors (the HTTP surface) are not checked — single-user.
	if actorType == "agent" {
		owns, err := s.AgentOwnsGoal(ctx, g, actorID)
		if err != nil {
			return nil, err
		}
		if !owns {
			return nil, NewValidationError("only the goal's current owner can hand it off")
		}
	}

	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET assignee_type=?, assignee_id=?, handoff_note=? WHERE id=?`,
		assigneeType, assigneeID, handoffNote, goalID); err != nil {
		return nil, fmt.Errorf("assign goal: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"from": g.AssigneeType + "/" + g.AssigneeID, "to": assigneeType + "/" + assigneeID})
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'handoff',?,?)`,
		newID(), goalID, actorType, actorID, string(detail), now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	// Handoff audit (决策 5-9): the append-only ownership-transition record,
	// richer than the activity row — from/to run links for the timeline and
	// the cycle counter. from_run_id = the old owner's running run (the one
	// the daemon terminates); to_run_id is back-filled by the caller via
	// CompleteHandoff after enqueuing the new owner's run.
	var fromRunID string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND status='running' ORDER BY started_at DESC LIMIT 1`,
		goalID).Scan(&fromRunID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find from_run for handoff: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO handoff_event (id,goal_id,from_type,from_id,to_type,to_id,from_run_id,to_run_id,reason,actor_type,actor_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), goalID, g.AssigneeType, g.AssigneeID, assigneeType, assigneeID, fromRunID, "", handoffNote, actorType, actorID, now()); err != nil {
		return nil, fmt.Errorf("insert handoff event: %w", err)
	}
	// Handoff-cycle detection (决策 5-7): ≥4 ownership transitions write a
	// system warning comment; ≥8 park the goal in review for a HUMAN decision
	// (approve continues, reject fails it) — a collaboration anomaly is not a
	// task failure, unlike the mention pingpong guard (决策 4-2).
	var handoffs int
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM handoff_event WHERE goal_id=?`, goalID).Scan(&handoffs); err != nil {
		return nil, fmt.Errorf("count handoffs: %w", err)
	}
	if handoffs == handoffWarnThreshold {
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
			newID(), goalID, fmt.Sprintf("⚠️ 所有权已交接 %d 次——协作循环风险。请确认任务方向，避免在 agent 之间来回推。", handoffs), now()); err != nil {
			return nil, fmt.Errorf("insert handoff warning: %w", err)
		}
	} else if handoffs >= handoffCycleThreshold {
		reason := fmt.Sprintf("handoff_loop: 所有权已交接 %d 次（超过上限 %d）——协作循环，需要人工决策", handoffs, handoffCycleThreshold)
		if res, err := s.st.DB().ExecContext(ctx,
			`UPDATE goal SET status='review', review_request=? WHERE id=? AND status='active'`, reason, goalID); err != nil {
			return nil, fmt.Errorf("park handoff loop: %w", err)
		} else if n, _ := res.RowsAffected(); n > 0 {
			if _, err := s.st.DB().ExecContext(ctx,
				`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,'system','','entered_review',?,?)`,
				newID(), goalID, `{"reason":"handoff_loop"}`, now()); err != nil {
				return nil, fmt.Errorf("insert handoff-loop activity: %w", err)
			}
			if _, err := s.st.DB().ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
				newID(), goalID, "任务停靠待审：agent 间所有权交接形成循环，请决定——批准继续（交接生效）/ 驳回（任务失败）。", now()); err != nil {
				return nil, fmt.Errorf("insert handoff-loop comment: %w", err)
			}
		}
	}
	// The handoff note is the actor's words — it belongs in the comment
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
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,NULL,?,?)`,
				newID(), goalID, actorType, actorID, content, now()); err != nil {
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

// CompleteHandoff back-fills the new owner's run id on the goal's latest
// handoff_event (决策 5-9) — the caller enqueues the new owner's run after
// Assign and calls this to close the audit row.
func (s *GoalService) CompleteHandoff(ctx context.Context, goalID, toRunID string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`UPDATE handoff_event SET to_run_id=? WHERE id=(
		   SELECT id FROM handoff_event WHERE goal_id=? AND to_run_id='' ORDER BY created_at DESC LIMIT 1)`,
		toRunID, goalID)
	return err
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
	// v2 (决策 6-8): cascade-cancel the goal's sub-goals — terminal ones keep
	// their history (verified/cancelled/failed stay), active work stops.
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='cancelled' WHERE goal_id=? AND status NOT IN ('verified','cancelled','failed')`, goalID); err != nil {
		return nil, fmt.Errorf("cancel sub-goals: %w", err)
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

// Delete removes a goal and dependents. The goal's running runs are NOT
// terminated here — their rows are gone by the time the daemon could act, so
// the ids are captured BEFORE the cascade and travel in the goal:deleted
// payload; the daemon cuts the processes (same mechanism as goal cancel,
// 决策 4-12). A deleted goal must not keep agents burning compute on work
// whose rows no longer exist.
func (s *GoalService) Delete(ctx context.Context, goalID string) error {
	runningRows, err := s.st.DB().QueryContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND status='running'`, goalID)
	if err != nil {
		return fmt.Errorf("collect running runs: %w", err)
	}
	var runningRunIDs []string
	for runningRows.Next() {
		var id string
		if err := runningRows.Scan(&id); err != nil {
			runningRows.Close()
			return err
		}
		runningRunIDs = append(runningRunIDs, id)
	}
	runningRows.Close()
	if err := runningRows.Err(); err != nil {
		return err
	}

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
		// v2 tables (决策 6-1/6-3/6-5) — FK order: revisions → changes →
		// verifications → sub-goals, then the collaboration audit rows.
		`DELETE FROM change_revision WHERE change_id IN (SELECT id FROM change WHERE goal_id=?)`,
		`DELETE FROM change WHERE goal_id=?`,
		`DELETE FROM verification_result WHERE goal_id=?`,
		`DELETE FROM sub_goal WHERE goal_id=?`,
		`DELETE FROM consult_request WHERE goal_id=?`,
		`DELETE FROM handoff_event WHERE goal_id=?`,
		`DELETE FROM goal WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, goalID); err != nil {
			return fmt.Errorf("delete goal dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:deleted", Payload: map[string]any{
		"goal_id": goalID, "run_ids": runningRunIDs,
	}})
	return nil
}

// AgentOwnsGoal reports whether agentID is the goal's current owner
// (决策 5-6, Collaboration.md Invariant 2): agent-assigned → the assignee;
// squad-assigned → the squad's CURRENT leader (dynamic, like ownRunByGoal);
// human-assigned → nobody (no agent owns it). The ownership permission anchor
// for handoff/consult/sub-goal MCP tools.
func (s *GoalService) AgentOwnsGoal(ctx context.Context, g *Goal, agentID string) (bool, error) {
	switch g.AssigneeType {
	case "agent":
		return g.AssigneeID == agentID, nil
	case "squad":
		var leaderID string
		err := s.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, g.AssigneeID).Scan(&leaderID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("load squad leader: %w", err)
		}
		return leaderID == agentID, nil
	default:
		return false, nil
	}
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
// terminal status (completed/failed/cancelled). Everything else — handoff,
// cancel — changes goal.status without consulting a run's result. See DESIGN.md
//
// Rules:
//
//	sub-goal runs report to the sub-goal layer (ReconcileSubGoalRun);
//	verify runs just land their report (ReconcileVerifyRun) — neither
//	  touches goal.status
//	run.agent != current assignee  → discard (handoff/reassign orphaned run)
//	goal.status == cancelled       → discard
//	run completed, no pending sub-goals/changes → gate judgment → done | review
//	run completed, work still pending → stay active (the Coordinator's
//	  attention loop owns the next wake, 决策 6-4/6-8)
//	run failed, attempts left → enqueue a retry run (attempt+1)
//	run failed, attempts exhausted → goal → failed
//	run cancelled → the goal stays where it is (timeout/handoff — 决策 2-6/6-6)
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
//
// The report THREADS to the run's trigger comment when one exists — the
// mention → run → answer chain (Collaboration.md §12: the guest's answer
// goes back to the requester; the frontend renders parent_id as a quote
// block). Owner runs (no trigger comment) land flat. Returns the inserted
// comment's id ("" when nothing was written) — the consult chain (决策 5-8)
// back-fills it as the response_comment_id.
func insertRunResultComment(ctx context.Context, tx *sql.Tx, rc goalRunContext) (string, error) {
	if rc.Status != "completed" || strings.TrimSpace(rc.Summary) == "" {
		return "", nil
	}
	id := newID()
	var parentID any
	if rc.TriggerCommentID != "" {
		parentID = rc.TriggerCommentID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,?,?,?,?,?,?)`,
		id, rc.GoalID, "agent", rc.AgentID, parentID, strings.TrimSpace(rc.Summary), now(), rc.RunID); err != nil {
		return "", fmt.Errorf("insert run-result comment: %w", err)
	}
	return id, nil
}

// ReconcileOnRunEnd runs under withBusyRetry for the same snapshot-upgrade
// reason as ReconcileGoal — a BUSY here would drop the reconcile AND the
// run.terminal publish (Finish returns the error), which is a worse failure
// than the retry's cost.
func (s *GoalService) ReconcileOnRunEnd(ctx context.Context, rc goalRunContext) error {
	return withBusyRetry(func() error { return s.reconcileOnRunEndOnce(ctx, rc) })
}

func (s *GoalService) reconcileOnRunEndOnce(ctx context.Context, rc goalRunContext) error {
	// Events are collected here and published ONLY after the tx commits, so a
	// failed commit can never leave the bus with an event whose DB change
	// rolled back (DESIGN "bus.Publish after commit"). Failure-side effects
	// that need a fresh transaction (retry enqueue) are also deferred to after
	// commit.
	var pendingEvents []events.Event
	var afterCommit []func()

	// Sub-goal runs report to the SUB-GOAL layer (决策 6-1): the goal's state
	// machine is not their business — a sub-goal run completing verifies the
	// work item, never the goal. Verify runs (决策 6-5) just land their report
	// — the verdict tool already made the transition.
	if rc.Role == "subgoal" {
		return s.ReconcileSubGoalRun(ctx, rc)
	}
	if rc.Role == "verify" {
		return s.ReconcileVerifyRun(ctx, rc)
	}

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
		return nil // goal vanished; nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("load goal for reconcile: %w", err)
	}
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
		reportID, err := insertRunResultComment(ctx, tx, rc)
		if err != nil {
			return err
		}
		// Consult closure (决策 5-8): a completed GUEST run answers a consult —
		// back-fill the response link, and auto-resume the requester (a fresh
		// run, attempt 1, no trigger comment — the resume must not stack the
		// mention-cycle counter) when the requester still owns an active goal.
		if rc.Status == "completed" {
			var requesterAgent, requesterRunID string
			err := tx.QueryRowContext(ctx,
				`SELECT requester_agent_id, requester_run_id FROM consult_request WHERE guest_run_id=?`,
				rc.RunID).Scan(&requesterAgent, &requesterRunID)
			if err == nil && requesterAgent != "" && reportID != "" {
				if _, uerr := tx.ExecContext(ctx,
					`UPDATE consult_request SET response_comment_id=? WHERE guest_run_id=?`,
					reportID, rc.RunID); uerr != nil {
					return fmt.Errorf("back-fill consult response: %w", uerr)
				}
				owns, oerr := s.AgentOwnsGoal(ctx, &g, requesterAgent)
				if oerr != nil {
					return oerr
				}
				if owns && g.Status == "active" {
					agent := requesterAgent
					afterCommit = append(afterCommit, func() {
						if err := s.runSvc.EnqueueExisting(ctx, rc.GoalID, agent, 1, false, ""); err != nil {
							log.Printf("goal: consult resume enqueue for %s: %v", rc.GoalID, err)
						}
					})
				}
			}
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
		if _, err := insertRunResultComment(ctx, tx, rc); err != nil {
			return err
		}
		// v2 finalization guard (决策 6-1/6-8): the owner run only REACHES
		// the acceptance judgment when nothing is pending — non-terminal
		// sub-goals (still working) or ready/conflicted changes (not yet
		// integrated) keep the goal active. The Coordinator's attention
		// spawn (run.terminal → ReconcileGoal) wakes the owner again for
		// integration/recovery; a sub-goal completing never completes the
		// goal, and the human gate fires only on the FINAL state.
		var pendingSG, pendingChanges int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sub_goal WHERE goal_id=? AND status NOT IN ('verified','cancelled','failed')`,
			rc.GoalID).Scan(&pendingSG); err != nil {
			return fmt.Errorf("count pending sub-goals: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM change WHERE goal_id=? AND status IN ('ready','conflict')`,
			rc.GoalID).Scan(&pendingChanges); err != nil {
			return fmt.Errorf("count pending changes: %w", err)
		}
		if pendingSG > 0 || pendingChanges > 0 {
			break // stay active — the attention loop owns the next step
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
				`UPDATE goal SET status='review', review_request=? WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
				reason, rc.GoalID)
			if err != nil {
				return fmt.Errorf("park goal in review: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				// The goal is not parkable (already review / terminal):
				// the gate cannot fire here. NO activity, NO
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
		// Clear the handoff/wakeup note as part of the same transaction that
		// promotes the goal — once this turn legitimately completes, the note
		// (a scoping instruction or child summary) is consumed. (See
		// DESIGN: the daemon no longer clears handoff_note itself; only the
		// goal layer, after confirming the run owns the goal, does.)
		if res, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='' WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
			rc.GoalID); err != nil {
			return fmt.Errorf("promote goal done: %w", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			break // the goal moved elsewhere (review/terminal raced) — no promote
		}
		// The goal reached a terminal state — queued runs (a mention that
		// raced ahead) must not be claimed onto a finished goal.
		if _, err := tx.ExecContext(ctx,
			`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, rc.GoalID); err != nil {
			return fmt.Errorf("cancel queued runs on done: %w", err)
		}
		// goal:finished fires ONLY when the goal actually reached a terminal
		// state (决策 5-10): review-parked / cancelled run ends emit no
		// goal-level event.
		pendingEvents = append(pendingEvents, events.Event{Topic: "goal:finished", Payload: map[string]any{
			"goal_id": rc.GoalID, "status": "done", "summary": rc.Summary,
		}})

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
				`UPDATE goal SET status='failed' WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
				rc.GoalID); err != nil {
				return fmt.Errorf("fail goal: %w", err)
			}
			// Terminal state — drop queued runs (same rule as the done path).
			if _, err := tx.ExecContext(ctx,
				`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, rc.GoalID); err != nil {
				return fmt.Errorf("cancel queued runs on fail: %w", err)
			}
			// The goal ACTUALLY failed here (attempts exhausted) — the failure
			// card fires once, not once per failed attempt (决策 5-10).
			pendingEvents = append(pendingEvents, events.Event{Topic: "goal:finished", Payload: map[string]any{
				"goal_id": rc.GoalID, "status": "failed", "summary": rc.Summary,
			}})
		}
	case "cancelled":
		// The run ended without completing or failing the goal: a timeout /
		// watchdog cut or a handoff cut (cancel_reason='handed_off', 决策 6-6).
		// The goal stays exactly where it is — no goal-level event (决策 5-10);
		// the daemon already published run:cancelled with the structured reason.
	}

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
// (The `request` gate is handled directly at the approval request site; it
// no longer exists — agents cannot request approval, 决策 4-11.)
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
	// A handoff_loop park (决策 5-7) has no deliver step at all — the
	// duplicate-decision guard (which protects the async merge) does not apply.
	handoffLoopPark := strings.HasPrefix(g.ReviewRequest, "handoff_loop:")
	if decision == "approve" || decision == "reject" || decision == "redirect" {
		var lastDecision string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT decision FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, goalID).
			Scan(&lastDecision)
		if err == nil && !deliverFailed && !handoffLoopPark {
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
	rule, err := s.resolveGateRule(ctx, goalID, runID)
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
		// Handoff-loop park (决策 5-7): there is nothing to deliver — the
		// handoff is already in effect. Approve releases the goal back to
		// active (the new owner's run proceeds); the deliver step must NOT
		// run for this park type.
		if strings.HasPrefix(g.ReviewRequest, "handoff_loop:") {
			if _, err := s.st.DB().ExecContext(ctx,
				`UPDATE goal SET status='active', review_request='' WHERE id=? AND status='review'`, goalID); err != nil {
				return nil, fmt.Errorf("release handoff loop: %w", err)
			}
			break
		}
		// Stay in review; the daemon's deliver step is the only mover from
		// here (MarkDelivered closes it).
		s.bus.Publish(ctx, events.Event{Topic: "goal:approved", Payload: map[string]any{
			"goal_id": goalID, "reason": reason,
		}})
	case "reject", "redirect":
		// Handoff-loop park: reject = the human stops the loop — the goal
		// fails (a human decision, not an automatic failure).
		if strings.HasPrefix(g.ReviewRequest, "handoff_loop:") {
			loopNote := "Handoff loop rejected"
			if reason != "" {
				loopNote += ": " + reason
			}
			if _, err := s.st.DB().ExecContext(ctx,
				`UPDATE goal SET status='failed', handoff_note=?, review_request='' WHERE id=? AND status='review'`,
				loopNote, goalID); err != nil {
				return nil, fmt.Errorf("fail handoff loop: %w", err)
			}
			if _, err := s.st.DB().ExecContext(ctx,
				`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
				return nil, fmt.Errorf("cancel queued runs on handoff-loop fail: %w", err)
			}
			s.bus.Publish(ctx, events.Event{Topic: "goal:finished", Payload: map[string]any{
				"goal_id": goalID, "status": "failed", "summary": loopNote,
			}})
			break
		}
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
		// Continue on the current assignee (the unified owner-run spawn, P0.5).
		// attempt resets to 1: a reject iteration is a fresh human-directed
		// cycle, not a machine retry — the reject count lives in
		// goal.human_iterations (DESIGN.md §4).
		if _, err := s.EnqueueOwnerRun(ctx, goalID); err != nil {
			return nil, fmt.Errorf("enqueue after reject: %w", err)
		}
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:review_resolved", Payload: map[string]any{
		"goal_id": goalID, "decision": decision,
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
	// Fresh run on the current assignee (the unified owner-run spawn, P0.5;
	// human-assigned goals stay manual — EnqueueOwnerRun no-ops for them).
	if _, err := s.EnqueueOwnerRun(ctx, goalID); err != nil {
		return nil, fmt.Errorf("enqueue after reopen: %w", err)
	}
	return s.Get(ctx, goalID)
}

// EnqueueOwnerRun is the SINGLE owner-run spawn entry (P0.5, 决策 6-4):
// every path that hands the goal's owner a fresh run — creation, handoff,
// reopen, reject, the Coordinator's attention spawn — goes through here.
// It is the conditional enqueue: active goals only, coalesced on the pending
// (goal, agent) pair — idempotent under event storms (running it twice still
// yields one run).
func (s *GoalService) EnqueueOwnerRun(ctx context.Context, goalID string) (*Run, error) {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if g.Status != "active" {
		return nil, nil // review/terminal goals take no fresh owner run
	}
	agentID, isLeader, squadID, err := s.runSvc.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, nil // human-assigned — no run to spawn
	}
	return s.runSvc.enqueue(ctx, goalID, agentID, 1, isLeader, squadID, "")
}

// EnqueueOwnerRunTx is EnqueueOwnerRun inside a caller's transaction (the
// Coordinator's spawn path).
func (s *GoalService) EnqueueOwnerRunTx(ctx context.Context, tx *sql.Tx, goalID string, assigneeType, assigneeID, status string) error {
	if status != "active" || (assigneeType != "agent" && assigneeType != "squad") {
		return nil // review/terminal/human goals take no fresh owner run
	}
	agentID, isLeader, squadID, err := s.runSvc.resolveLeader(ctx, assigneeType, assigneeID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return nil
	}
	_, _, err = s.runSvc.EnqueueExistingTx(ctx, tx, goalID, agentID, 1, isLeader, squadID)
	return err
}

// ReconcileGoal is the Coordinator core (决策 6-4): events are only WAKEUP
// HINTS — the authoritative decision is recomputed from DB state inside ONE
// transaction, and every transition is conditional (running ReconcileGoal 100
// times yields the same end state). The attention derivation covers
// sub-goal/change state (决策 6-1/6-3 — the predicates grow with the sub-goal
// flow). Owner run creation goes through this path ONLY (P0.5).
//
// The body runs under withBusyRetry: a WAL snapshot-upgrade race fires
// SQLITE_BUSY immediately (busy_timeout does not apply), and because the
// transaction is idempotent the retry is sound (the live 16:13:07
// "persist attention: database is locked (5)" failure is exactly this).
func (s *GoalService) ReconcileGoal(ctx context.Context, goalID string) error {
	unlock := s.lockReconcile(goalID)
	defer unlock()
	return withBusyRetry(func() error { return s.reconcileGoalOnce(ctx, goalID) })
}

func (s *GoalService) reconcileGoalOnce(ctx context.Context, goalID string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var assigneeType, assigneeID, status string
	err = tx.QueryRowContext(ctx,
		`SELECT assignee_type, assignee_id, status FROM goal WHERE id=?`, goalID).
		Scan(&assigneeType, &assigneeID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // goal vanished — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("load goal for reconcile: %w", err)
	}

	attention, err := s.deriveOwnerAttentionTx(ctx, tx, goalID)
	if err != nil {
		return err
	}
	// The derived attention is persisted (决策 6-8): the UI badge and the IM
	// notification dedup read it; human owners get NO run — attention + notify.
	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET attention=? WHERE id=?`, attention, goalID); err != nil {
		return fmt.Errorf("persist attention: %w", err)
	}

	// Owner needed AND the goal can take a run AND the owner is idle → one
	// conditional enqueue (coalesced on the pending (goal, agent) pair — the
	// idempotency guard; two racing reconciles still produce ONE run).
	// Loop guards:
	//   - a CANCELLED owner run (timeout/handoff) leaves the goal active with
	//     attention still pending — auto-spawning would loop the stuck agent
	//     (决策 2-6: a timeout is the human's call);
	//   - PROGRESS guard: spawn only when an attention SIGNAL (a change
	//     revision or a failed sub-goal's run) is NEWER than the goal's last
	//     owner-run spawn — an owner woken for THIS set of changes that made
	//     no progress must not be re-spawned in a loop (the E2E spun 7 wakes).
	//     A new revision / failure re-arms the spawn.
	lastOwnerCancelled := false
	var newestSignal, lastOwnerSpawn string
	if attention != "" {
		var lastStatus string
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM run WHERE goal_id=? AND role='owner' ORDER BY created_at DESC LIMIT 1`,
			goalID).Scan(&lastStatus)
		if err == nil && lastStatus == "cancelled" {
			lastOwnerCancelled = true
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT MAX(x.ts) FROM (
			  SELECT COALESCE((SELECT MAX(r2.created_at) FROM change_revision r2 WHERE r2.change_id = c.id), c.created_at) AS ts
			  FROM change c WHERE c.goal_id=? AND c.status IN ('ready','conflict')
			  UNION ALL
			  SELECT MAX(COALESCE((SELECT MAX(r3.finished_at) FROM run r3 WHERE r3.sub_goal_id = sg.id AND r3.role='subgoal'), sg.created_at))
			  FROM sub_goal sg WHERE sg.goal_id=? AND sg.status='failed'
			) x`, goalID, goalID).Scan(&newestSignal); err != nil {
			return fmt.Errorf("attention signal recency: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(queued_at),'') FROM run WHERE goal_id=? AND role='owner'`,
			goalID).Scan(&lastOwnerSpawn); err != nil {
			return fmt.Errorf("last owner spawn: %w", err)
		}
	}
	if attention != "" && !lastOwnerCancelled && newestSignal > lastOwnerSpawn {
		if err := s.EnqueueOwnerRunTx(ctx, tx, goalID, assigneeType, assigneeID, status); err != nil {
			return fmt.Errorf("reconcile enqueue owner: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Human owners get NO run — the attention event drives IM + the UI badge
	// (决策 6-8). Published after commit (invariant 13).
	if attention != "" && assigneeType == "human" {
		s.bus.Publish(ctx, events.Event{Topic: "goal.attention_needed", Payload: map[string]any{
			"goal_id": goalID, "attention": attention,
		}})
	}
	return nil
}

// deriveOwnerAttentionTx computes the v2 OwnerAttention bits (决策 6-4/6-8)
// from authoritative DB state. Empty = nothing needs the owner.
func (s *GoalService) deriveOwnerAttentionTx(ctx context.Context, tx *sql.Tx, goalID string) (string, error) {
	var bits []string
	// need_recovery: a sub-goal failed — the owner decides (cancel/retry/reassign).
	var failed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sub_goal WHERE goal_id=? AND status='failed'`, goalID).Scan(&failed); err != nil {
		return "", fmt.Errorf("attention: sub-goal failed: %w", err)
	}
	if failed > 0 {
		bits = append(bits, "recovery")
	}
	// need_integration: changes ready for the owner to merge.
	var ready int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change WHERE goal_id=? AND status IN ('ready','conflict')`, goalID).Scan(&ready); err != nil {
		return "", fmt.Errorf("attention: changes ready: %w", err)
	}
	if ready > 0 {
		bits = append(bits, "integration")
	}
	return strings.Join(bits, ","), nil
}

// resolveGateRule names the rule that actually parked the goal in review:
//   - the named run's gates_hit[0] (the IM card carries the evidence run),
//   - else the goal's latest completed run's gates_hit[0] (the Web panel),
//   - else "merge" (the strength-linkage default gate fires with empty
//     gates_hit).
func (s *GoalService) resolveGateRule(ctx context.Context, goalID, runID string) (string, error) {
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
	// No gates_hit recorded — the strength-linkage default gate (weak domain)
	// or the unfrozen-policy checkpoint fired with empty gates_hit.
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
//	success → review → done (handoff_note cleared)
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

	var statusCheck string
	err = tx.QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, goalID).Scan(&statusCheck)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load goal: %w", err)
	}
	_ = statusCheck

	event := "goal:deliver_failed"
	if success {
		// A delivered goal is done: the deliver already merged this goal's
		// branch. Pending sub-goals cannot exist here — the finalization guard
		// (决策 6-8) keeps the goal out of the gate judgment while work is
		// pending.
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='', review_request='' WHERE id=? AND status='review'`, goalID); err != nil {
			return nil, fmt.Errorf("mark delivered: %w", err)
		}
		// Terminal state — drop queued runs (same rule as the reconcile paths).
		if _, err := tx.ExecContext(ctx,
			`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
			return nil, fmt.Errorf("cancel queued runs on delivered: %w", err)
		}
		event = "goal:delivered"
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

// isSQLiteBusy reports whether err is a SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT
// failure. WAL snapshot-upgrade races fail IMMEDIATELY with SQLITE_BUSY —
// the busy_timeout pragma does not apply to them (the tx took a read
// snapshot, another writer committed, and the write upgrade collides). The
// live failure: ReconcileGoal's attention persist hit (5) SQLITE_BUSY at
// exactly the moment the review-run enqueue committed alongside it.
func isSQLiteBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_BUSY || se.Code() == sqlite3.SQLITE_BUSY_SNAPSHOT
	}
	return false
}

// withBusyRetry re-runs fn when it fails with SQLITE_BUSY. Safe ONLY for
// callers whose transaction is a conditional transition (rolled back on
// failure, so the retry re-runs from a fresh snapshot) — the reconcile
// family is idempotent by design (决策 6-4), which is exactly what makes the
// retry sound.
func withBusyRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = fn()
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
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
