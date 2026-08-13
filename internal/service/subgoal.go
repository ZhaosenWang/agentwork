package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/eushing/agentwork/internal/events"
)

// SubGoal is a work item split off a goal (v2 model, 决策 6-1) — NOT a child
// goal: no parent recursion, no own deliver/verification terminal semantics.
// The goal stays the sole lifecycle authority; a sub-goal carries work
// responsibility (assignee) + quality responsibility (verifier, ” = machine).
// execution_attempt (machine retries ≤3) and quality_iteration (verifier
// rejects, unbounded) are the authoritative counters (决策 6-9) — run.attempt
// is just the instance ordinal.
type SubGoal struct {
	ID               string `json:"id"`
	GoalID           string `json:"goal_id"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	AssigneeID       string `json:"assignee_id"`
	VerifierID       string `json:"verifier_id"` // '' = machine (domain verify commands)
	Status           string `json:"status"`      // pending|running|done|verifying|verified|rejected|cancelled|failed
	ExecutionAttempt int    `json:"execution_attempt"`
	QualityIteration int    `json:"quality_iteration"`
	CreatedAt        string `json:"created_at"`
}

// maxExecutionAttempts bounds the sub-goal's machine retry chain (决策 6-5,
// the execution_attempt counter — verification failures and run failures
// share it; verifier rejects use quality_iteration instead).
const maxExecutionAttempts = 3

// maxActiveSubGoals bounds concurrent work items per goal (决策 6-8: fan-out
// ≤20 ACTIVE — history is unlimited, cancelled/failed/verified don't count).
const maxActiveSubGoals = 20

// CreateSubGoal splits a work item off a goal (决策 6-1): a sub_goal row with
// its own assignee (an agent), started running immediately with a sub-goal
// run enqueued. The goal's owner/status are untouched. Only the goal's owner
// (agent run), a squad leader, or a human may create (checked by the caller —
// the MCP tool's requireOwnerOf; the service trusts the caller identity).
func (s *GoalService) CreateSubGoal(ctx context.Context, goalID, title, description, assigneeID, verifierID, createdByType, createdByID string) (*SubGoal, error) {
	if title == "" {
		return nil, NewValidationError("title is required")
	}
	if assigneeID == "" {
		return nil, NewValidationError("assignee_id is required")
	}
	// Fan-out cap (决策 6-8): at most maxActiveSubGoals non-terminal sub-goals
	// per goal — an agent must not turn one goal into an unbounded work farm.
	var active int
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sub_goal WHERE goal_id=? AND status NOT IN ('verified','cancelled','failed')`,
		goalID).Scan(&active); err != nil {
		return nil, fmt.Errorf("count active sub-goals: %w", err)
	}
	if active >= maxActiveSubGoals {
		return nil, NewValidationError(fmt.Sprintf("too many active sub-goals (%d max) — resolve existing ones first", maxActiveSubGoals))
	}

	// The goal must exist and be one a sub-goal can hang off (active or
	// review — the owner splits work from either).
	var gStatus string
	err := s.st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, goalID).Scan(&gStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load goal: %w", err)
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, assigneeID, "assignee agent"); err != nil {
		return nil, err
	}
	if verifierID != "" {
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, verifierID, "verifier agent"); err != nil {
			return nil, err
		}
	}
	ts := now()
	sg := SubGoal{
		ID:          newID(),
		GoalID:      goalID,
		Title:       title,
		Description: description,
		AssigneeID:  assigneeID,
		VerifierID:  verifierID,
		Status:      "running",
		CreatedAt:   ts,
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO sub_goal (id,goal_id,title,description,assignee_id,verifier_id,status,execution_attempt,quality_iteration,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sg.ID, sg.GoalID, sg.Title, sg.Description, sg.AssigneeID, sg.VerifierID, sg.Status, sg.ExecutionAttempt, sg.QualityIteration, sg.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert sub_goal: %w", err)
	}
	if s.runSvc == nil {
		return nil, errors.New("goalSvc.runSvc not wired")
	}
	if _, err := s.runSvc.EnqueueSubGoalRun(ctx, sg.ID); err != nil {
		return nil, fmt.Errorf("enqueue sub-goal run: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "sub_goal.created", Payload: map[string]any{
		"goal_id": goalID, "sub_goal_id": sg.ID,
	}})
	return &sg, nil
}

// GetSubGoal loads one sub-goal row.
func (s *GoalService) GetSubGoal(ctx context.Context, id string) (*SubGoal, error) {
	var sg SubGoal
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,goal_id,title,description,assignee_id,verifier_id,status,execution_attempt,quality_iteration,created_at
		 FROM sub_goal WHERE id=?`, id).
		Scan(&sg.ID, &sg.GoalID, &sg.Title, &sg.Description, &sg.AssigneeID, &sg.VerifierID, &sg.Status, &sg.ExecutionAttempt, &sg.QualityIteration, &sg.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sg, nil
}

// ListSubGoals lists a goal's sub-goals (oldest first).
func (s *GoalService) ListSubGoals(ctx context.Context, goalID string) ([]SubGoal, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,goal_id,title,description,assignee_id,verifier_id,status,execution_attempt,quality_iteration,created_at
		 FROM sub_goal WHERE goal_id=? ORDER BY created_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubGoal{}
	for rows.Next() {
		var sg SubGoal
		if err := rows.Scan(&sg.ID, &sg.GoalID, &sg.Title, &sg.Description, &sg.AssigneeID, &sg.VerifierID, &sg.Status, &sg.ExecutionAttempt, &sg.QualityIteration, &sg.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// ReconcileSubGoalRun is the sub-goal layer's arbitration (决策 6-1/6-3/6-5):
// a sub-goal run's terminal outcome advances the SUB-GOAL, never the goal.
//
//	completed → the daemon already ran machine verification (a completed run
//	            reached here only after it passed) → stamp verified + create
//	            the Change and its first Revision ATOMICALLY (Ready ⇔ a
//	            persisted Revision) — the daemon stamped base_ref/head_ref.
//	failed    → execution_attempt++, retry the assignee (≤3), else failed.
//	cancelled → same bounded retry chain (a watchdog stall is a machine
//	            hiccup for a sub-goal — the goal-level "human decides" loop
//	            exists for owner runs; sub-goal retries are cheap and bounded).
func (s *GoalService) ReconcileSubGoalRun(ctx context.Context, rc goalRunContext) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sg SubGoal
	err = tx.QueryRowContext(ctx,
		`SELECT id,goal_id,title,description,assignee_id,verifier_id,status,execution_attempt,quality_iteration,created_at
		 FROM sub_goal WHERE id=?`, rc.SubGoalID).
		Scan(&sg.ID, &sg.GoalID, &sg.Title, &sg.Description, &sg.AssigneeID, &sg.VerifierID, &sg.Status, &sg.ExecutionAttempt, &sg.QualityIteration, &sg.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // sub-goal vanished — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("load sub-goal for reconcile: %w", err)
	}

	var evs []events.Event
	enqueueVerifierAfterCommit := false
	enqueueRetryAfterCommit := false
	switch rc.Status {
	case "completed":
		// The run's report lands in the goal's feed (owner context).
		if _, err := insertRunResultComment(ctx, tx, rc); err != nil {
			return err
		}
		// An agent verifier was named (决策 6-5): machine checks passed, now
		// the QUALITY gate — park in verifying and enqueue the verifier run.
		// The verdict tool (verify_sub_goal) makes the verified/rejected
		// transition; this run's job ends here.
		if sg.VerifierID != "" {
			res, err := tx.ExecContext(ctx,
				`UPDATE sub_goal SET status='verifying' WHERE id=? AND status IN ('done','running')`, sg.ID)
			if err != nil {
				return fmt.Errorf("park sub-goal verifying: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				break // already terminal — no change
			}
			evs = append(evs, events.Event{Topic: "sub_goal.verifying", Payload: map[string]any{
				"goal_id": sg.GoalID, "sub_goal_id": sg.ID,
			}})
			enqueueVerifierAfterCommit = true
			break
		}
		// Conditional transition: done → verified. (The daemon's verification
		// gate guarantees a completed run passed machine checks.)
		res, err := tx.ExecContext(ctx,
			`UPDATE sub_goal SET status='verified' WHERE id=? AND status IN ('done','running')`, sg.ID)
		if err != nil {
			return fmt.Errorf("verify sub-goal: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			break // already terminal (cancelled/rejected raced) — no change
		}
		changeID, err := s.materializeChangeTx(ctx, tx, sg, rc.BaseRef, rc.HeadRef)
		if err != nil {
			return err
		}
		evs = append(evs, events.Event{Topic: "sub_goal.verified", Payload: map[string]any{
			"goal_id": sg.GoalID, "sub_goal_id": sg.ID, "change_id": changeID,
		}})
		// The change is now READY for integration (v2 §11: change.ready — the
		// frontend change panel and the attention badge react to it). A
		// no-code sub-goal (materialize returned '') has no change to report.
		if changeID != "" {
			evs = append(evs, events.Event{Topic: "change.ready", Payload: map[string]any{
				"goal_id": sg.GoalID, "change_id": changeID, "sub_goal_id": sg.ID,
			}})
		}
	case "failed", "cancelled":
		if _, err := tx.ExecContext(ctx,
			`UPDATE sub_goal SET execution_attempt=execution_attempt+1 WHERE id=? AND status IN ('running','done')`,
			sg.ID); err != nil {
			return fmt.Errorf("bump execution_attempt: %w", err)
		}
		var attempt int
		_ = tx.QueryRowContext(ctx, `SELECT execution_attempt FROM sub_goal WHERE id=?`, sg.ID).Scan(&attempt)
		if attempt < maxExecutionAttempts {
			// Retry the assignee (a fresh sub-goal run, same sub-goal).
			evs = append(evs, events.Event{Topic: "sub_goal.retrying", Payload: map[string]any{
				"goal_id": sg.GoalID, "sub_goal_id": sg.ID, "execution_attempt": attempt,
			}})
			enqueueRetryAfterCommit = true
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE sub_goal SET status='failed' WHERE id=? AND status IN ('running','done')`, sg.ID); err != nil {
				return fmt.Errorf("fail sub-goal: %w", err)
			}
			evs = append(evs, events.Event{Topic: "sub_goal.failed", Payload: map[string]any{
				"goal_id": sg.GoalID, "sub_goal_id": sg.ID, "execution_attempt": attempt,
			}})
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	// Post-commit side effects (invariant 13) — explicit, not deferred: a
	// failed commit must never enqueue.
	if enqueueVerifierAfterCommit {
		if _, err := s.runSvc.EnqueueVerifyRun(ctx, sg.ID); err != nil {
			log.Printf("sub-goal verifier enqueue %s: %v", sg.ID, err)
		}
	}
	if enqueueRetryAfterCommit {
		if _, err := s.runSvc.EnqueueSubGoalRun(ctx, sg.ID); err != nil {
			log.Printf("sub-goal retry enqueue %s: %v", sg.ID, err)
		}
	}
	for _, e := range evs {
		s.bus.Publish(ctx, e)
	}
	return nil
}

// materializeChangeTx makes the sub-goal's deliverable concrete (决策 6-3),
// called inside the verifying/verified transition's transaction:
//
//	no-code sub-goal (决策 6-8): head empty or base == head — the deliverable
//	  is the run's report in the feed, NO Change is born ('' returned);
//	conflict rework: the sub-goal's previous Change was conflicted at
//	  integration — append revision seq N+1 on the SAME change (base = the
//	  new integration base) and return it to ready;
//	fresh round: create the Change and its first Revision atomically
//	  (Ready ⇔ a persisted Revision).
func (s *GoalService) materializeChangeTx(ctx context.Context, tx *sql.Tx, sg SubGoal, baseRef, headRef string) (string, error) {
	if headRef == "" || baseRef == headRef {
		return "", nil
	}
	var conflictChangeID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM change WHERE sub_goal_id=? AND status='conflict' ORDER BY created_at DESC LIMIT 1`,
		sg.ID).Scan(&conflictChangeID)
	if err == nil && conflictChangeID != "" {
		var seq int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq),0) FROM change_revision WHERE change_id=?`, conflictChangeID).Scan(&seq); err != nil {
			return "", fmt.Errorf("load revision seq: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO change_revision (id,change_id,seq,base_ref,head_ref,created_at) VALUES (?,?,?,?,?,?)`,
			newID(), conflictChangeID, seq+1, baseRef, headRef, now()); err != nil {
			return "", fmt.Errorf("append change revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE change SET status='ready' WHERE id=? AND status='conflict'`, conflictChangeID); err != nil {
			return "", fmt.Errorf("return change to ready: %w", err)
		}
		return conflictChangeID, nil
	}
	changeID := newID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO change (id,goal_id,sub_goal_id,status,created_at) VALUES (?,?,?,'ready',?)`,
		changeID, sg.GoalID, sg.ID, now()); err != nil {
		return "", fmt.Errorf("insert change: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO change_revision (id,change_id,seq,base_ref,head_ref,created_at) VALUES (?,?,?,?,?,?)`,
		newID(), changeID, 1, baseRef, headRef, now()); err != nil {
		return "", fmt.Errorf("insert change revision: %w", err)
	}
	return changeID, nil
}

// VerifySubGoal is the verifier verdict tool's service entry (决策 6-5): the
// CALLING run must be the sub-goal's verify run (role + sub_goal_id match —
// the verdict authority lives in the run, not the tool caller's say-so).
// passed → verified + Change/Revision atomic + sub_goal.verified event;
// rejected → verification_result recorded + quality_iteration++ + back to
// running with the assignee's successor run. Every round appends a
// verification_result row (audit/evidence).
func (s *GoalService) VerifySubGoal(ctx context.Context, runID, verdict, summary, evidence string) error {
	if verdict != "passed" && verdict != "rejected" {
		return NewValidationError("verdict must be passed or rejected")
	}
	var role, subGoalID, agentID, goalID string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT role, sub_goal_id, agent_id, goal_id FROM run WHERE id=?`, runID).
		Scan(&role, &subGoalID, &agentID, &goalID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load verify run: %w", err)
	}
	if role != "verify" {
		return NewValidationError("only a verify run can issue a sub-goal verdict")
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sg SubGoal
	err = tx.QueryRowContext(ctx,
		`SELECT id,goal_id,title,description,assignee_id,verifier_id,status,execution_attempt,quality_iteration,created_at
		 FROM sub_goal WHERE id=?`, subGoalID).
		Scan(&sg.ID, &sg.GoalID, &sg.Title, &sg.Description, &sg.AssigneeID, &sg.VerifierID, &sg.Status, &sg.ExecutionAttempt, &sg.QualityIteration, &sg.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load sub-goal: %w", err)
	}
	// Conditional: only a verifying sub-goal takes a verdict (idempotency —
	// double verdicts from a retried tool call no-op).
	if sg.Status != "verifying" {
		return NewValidationError("the sub-goal is not awaiting a verdict")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO verification_result (id,goal_id,sub_goal_id,verifier_run_id,status,summary,evidence,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		newID(), sg.GoalID, sg.ID, runID, verdict, summary, evidence, now()); err != nil {
		return fmt.Errorf("insert verification_result: %w", err)
	}

	var evs []events.Event
	enqueueRejectAfterCommit := false
	if verdict == "passed" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sub_goal SET status='verified' WHERE id=? AND status='verifying'`, sg.ID); err != nil {
			return fmt.Errorf("verify sub-goal: %w", err)
		}
		// The revision refs come from the ASSIGNEE's latest run (the work
		// being judged), not the verifier run.
		var baseRef, headRef string
		if err := tx.QueryRowContext(ctx,
			`SELECT base_ref, head_ref FROM run WHERE sub_goal_id=? AND role='subgoal' ORDER BY finished_at DESC LIMIT 1`,
			sg.ID).Scan(&baseRef, &headRef); err != nil {
			return fmt.Errorf("load assignee refs: %w", err)
		}
		changeID, err := s.materializeChangeTx(ctx, tx, sg, baseRef, headRef)
		if err != nil {
			return err
		}
		evs = append(evs, events.Event{Topic: "sub_goal.verified", Payload: map[string]any{
			"goal_id": sg.GoalID, "sub_goal_id": sg.ID, "change_id": changeID,
		}})
		if changeID != "" {
			evs = append(evs, events.Event{Topic: "change.ready", Payload: map[string]any{
				"goal_id": sg.GoalID, "change_id": changeID, "sub_goal_id": sg.ID,
			}})
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sub_goal SET status='running', quality_iteration=quality_iteration+1 WHERE id=? AND status='verifying'`,
			sg.ID); err != nil {
			return fmt.Errorf("reject sub-goal: %w", err)
		}
		evs = append(evs, events.Event{Topic: "sub_goal.rejected", Payload: map[string]any{
			"goal_id": sg.GoalID, "sub_goal_id": sg.ID,
		}})
		enqueueRejectAfterCommit = true
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Post-commit side effect (invariant 13) — explicit, not deferred.
	if enqueueRejectAfterCommit {
		if _, err := s.runSvc.EnqueueSubGoalRun(ctx, sg.ID); err != nil {
			log.Printf("sub-goal reject enqueue %s: %v", sg.ID, err)
		}
	}
	for _, e := range evs {
		s.bus.Publish(ctx, e)
	}
	return nil
}

// MarkChangeIntegrated records an integration attempt's outcome (决策 6-3):
// the git merge itself runs in the owner's run workspace (the integrate_change
// tool); this stamps the authoritative Change state. success → integrated;
// conflict → conflict + the sub-goal goes back to running (quality_iteration++
// — the work was rejected by integration reality) with the assignee's
// successor run enqueued; a later revision appends change_revision seq N+1.
func (s *GoalService) MarkChangeIntegrated(ctx context.Context, changeID string, success bool) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sgID, goalID string
	if err := tx.QueryRowContext(ctx,
		`SELECT sub_goal_id, goal_id FROM change WHERE id=?`, changeID).Scan(&sgID, &goalID); err != nil {
		return fmt.Errorf("load change: %w", err)
	}
	if success {
		if _, err := tx.ExecContext(ctx,
			`UPDATE change SET status='integrated' WHERE id=? AND status IN ('ready','integrating','conflict')`,
			changeID); err != nil {
			return fmt.Errorf("mark integrated: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.bus.Publish(ctx, events.Event{Topic: "change.integrated", Payload: map[string]any{
			"goal_id": goalID, "change_id": changeID, "sub_goal_id": sgID,
		}})
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE change SET status='conflict' WHERE id=? AND status IN ('ready','integrating')`,
		changeID); err != nil {
		return fmt.Errorf("mark conflict: %w", err)
	}
	// The sub-goal goes back to running for the assignee's rework round.
	if _, err := tx.ExecContext(ctx,
		`UPDATE sub_goal SET status='running', quality_iteration=quality_iteration+1 WHERE id=? AND status='verified'`,
		sgID); err != nil {
		return fmt.Errorf("rework sub-goal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The assignee's successor run — after commit (invariant 13).
	if _, err := s.runSvc.EnqueueSubGoalRun(ctx, sgID); err != nil {
		log.Printf("sub-goal conflict enqueue %s: %v", sgID, err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "change.conflict", Payload: map[string]any{
		"goal_id": goalID, "change_id": changeID, "sub_goal_id": sgID,
	}})
	return nil
}

// ReconcileVerifyRun closes a verify run (决策 6-5): the verdict tool already
// made the sub-goal transition — the run itself just lands its report in the
// feed and is discarded (verify runs have no goal authority).
func (s *GoalService) ReconcileVerifyRun(ctx context.Context, rc goalRunContext) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := insertRunResultComment(ctx, tx, rc); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "run:discarded", Payload: map[string]any{
		"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "verify-run",
	}})
	return nil
}

// Change is a logical deliverable produced by a sub-goal (决策 6-3). HeadRef
// is the LATEST revision's head — the ref the owner integrates.
type Change struct {
	ID        string `json:"id"`
	GoalID    string `json:"goal_id"`
	SubGoalID string `json:"sub_goal_id"`
	Status    string `json:"status"` // ready|integrating|integrated|conflict
	HeadRef   string `json:"head_ref"`
	CreatedAt string `json:"created_at"`
}

// GetChange loads a change with its latest revision's head ref.
func (s *GoalService) GetChange(ctx context.Context, changeID string) (*Change, error) {
	var c Change
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT c.id, c.goal_id, c.sub_goal_id, c.status, c.created_at,
		        COALESCE((SELECT r.head_ref FROM change_revision r WHERE r.change_id = c.id ORDER BY r.seq DESC LIMIT 1), '')
		 FROM change c WHERE c.id=?`, changeID).
		Scan(&c.ID, &c.GoalID, &c.SubGoalID, &c.Status, &c.CreatedAt, &c.HeadRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListChanges lists a goal's changes (for the owner's resume context).
func (s *GoalService) ListChanges(ctx context.Context, goalID string) ([]Change, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT c.id, c.goal_id, c.sub_goal_id, c.status, c.created_at,
		        COALESCE((SELECT r.head_ref FROM change_revision r WHERE r.change_id = c.id ORDER BY r.seq DESC LIMIT 1), '')
		 FROM change c WHERE c.goal_id=? ORDER BY c.created_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.GoalID, &c.SubGoalID, &c.Status, &c.CreatedAt, &c.HeadRef); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChangeRevision is one revision of a Change (决策 6-3): every revision binds
// the change to the integration base it was built against (base_ref) and the
// commit that delivers it (head_ref). Conflict rework appends seq N+1 on the
// SAME change — the revision history is the audit of which base each round
// targeted.
type ChangeRevision struct {
	ID        string `json:"id"`
	ChangeID  string `json:"change_id"`
	Seq       int    `json:"seq"`
	BaseRef   string `json:"base_ref"`
	HeadRef   string `json:"head_ref"`
	CreatedAt string `json:"created_at"`
}

// ChangeDetail is a Change with its revision history — the Web change
// panel's row (the owner's integration view).
type ChangeDetail struct {
	Change
	Revisions []ChangeRevision `json:"revisions"`
}

// ListChangeDetails lists a goal's changes with their revisions. A
// per-change revision query is fine at single-user scale (at most
// maxActiveSubGoals changes per goal).
func (s *GoalService) ListChangeDetails(ctx context.Context, goalID string) ([]ChangeDetail, error) {
	changes, err := s.ListChanges(ctx, goalID)
	if err != nil {
		return nil, err
	}
	out := make([]ChangeDetail, 0, len(changes))
	for _, c := range changes {
		rows, err := s.st.DB().QueryContext(ctx,
			`SELECT id, change_id, seq, base_ref, head_ref, created_at
			 FROM change_revision WHERE change_id=? ORDER BY seq`, c.ID)
		if err != nil {
			return nil, fmt.Errorf("list revisions: %w", err)
		}
		var revs []ChangeRevision
		for rows.Next() {
			var r ChangeRevision
			if err := rows.Scan(&r.ID, &r.ChangeID, &r.Seq, &r.BaseRef, &r.HeadRef, &r.CreatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan revision: %w", err)
			}
			revs = append(revs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out = append(out, ChangeDetail{Change: c, Revisions: revs})
	}
	return out, nil
}

// MarkChangeIntegrating stamps Ready → Integrating (the integrate_change tool
// merges next; conditional so a raced duplicate no-ops).
func (s *GoalService) MarkChangeIntegrating(ctx context.Context, changeID string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE change SET status='integrating' WHERE id=? AND status='ready'`, changeID); err != nil {
		return fmt.Errorf("mark integrating: %w", err)
	}
	return nil
}

// VerificationResult is one verification round of a sub-goal (决策 6-5) —
// the audit row the get_verification tool expands from the resume index.
type VerificationResult struct {
	ID            string `json:"id"`
	GoalID        string `json:"goal_id"`
	SubGoalID     string `json:"sub_goal_id"`
	VerifierRunID string `json:"verifier_run_id"`
	Status        string `json:"status"` // passed|rejected
	Summary       string `json:"summary"`
	Evidence      string `json:"evidence"`
	CreatedAt     string `json:"created_at"`
}

// ListVerificationResults lists a sub-goal's verification rounds (newest first).
func (s *GoalService) ListVerificationResults(ctx context.Context, subGoalID string) ([]VerificationResult, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,goal_id,sub_goal_id,verifier_run_id,status,summary,evidence,created_at
		 FROM verification_result WHERE sub_goal_id=? ORDER BY created_at DESC`, subGoalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VerificationResult{}
	for rows.Next() {
		var v VerificationResult
		if err := rows.Scan(&v.ID, &v.GoalID, &v.SubGoalID, &v.VerifierRunID, &v.Status, &v.Summary, &v.Evidence, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CancelSubGoal cancels a sub-goal (owner management, 决策 6-1): conditional
// transition on non-terminal states; the sub-goal's queued run is dropped and
// the daemon kills a running one (sub_goal.cancelled event → runCancels).
func (s *GoalService) CancelSubGoal(ctx context.Context, subGoalID string) (*SubGoal, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='cancelled' WHERE id=? AND status NOT IN ('verified','cancelled','failed')`,
		subGoalID)
	if err != nil {
		return nil, fmt.Errorf("cancel sub-goal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, NewValidationError("the sub-goal is already terminal")
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled' WHERE sub_goal_id=? AND status='queued'`, subGoalID); err != nil {
		return nil, fmt.Errorf("cancel sub-goal queued runs: %w", err)
	}
	sg, err := s.GetSubGoal(ctx, subGoalID)
	if err != nil {
		return nil, err
	}
	s.bus.Publish(ctx, events.Event{Topic: "sub_goal.cancelled", Payload: map[string]any{
		"goal_id": sg.GoalID, "sub_goal_id": sg.ID,
	}})
	return sg, nil
}
