package daemon

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/google/uuid"
)

// ── Squad review checkpoint (platform-enforced, not agent discretion) ──
//
// The reviewer is a member of the squad — the squad OWNS the "who reviews"
// rule (member role="reviewer", set when the squad is built), and the
// PLATFORM enforces it: when a squad-owned goal parks in review (a gate
// fired on the completed owner run), the daemon automatically posts a
// system mention asking each reviewer to review the change. No prompt text
// has to tell the leader to "hand off to the reviewer" — the squad's
// structure IS the instruction, and it fires every round (reject → new
// leader run → review again → reviewers re-triggered).
//
// The review run is an ordinary mention run (EnqueueForMention): it runs in
// its own read-only worktree while the goal sits in review (the human's
// approval window — that is exactly when the review opinions must be
// visible), and its result is discarded by reconcile like any guest run —
// the opinions live in the comments, which the approval card and the human
// read.
//
// Note: goal:reviewing is published only by the gate park in
// ReconcileOnRunEnd (a handoff-loop park, 决策 5-7, emits no event — the
// squad has no code change to review there). The coalesce on (goal,
// reviewer) keeps re-parks from stacking runs.

// onGoalReviewing reacts to a goal parking in review: if the goal belongs to
// a squad with reviewer members, trigger the squad's review checkpoint, then
// open the reviewer-first approval window (Option B): the human's card fires
// on goal:review_ready — after this window's review runs are terminal — not
// at park time with an empty opinion section.
func (d *Daemon) onGoalReviewing(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	// NOTE: event handlers must NOT use the published event's ctx — it is the
	// PUBLISHER's ctx (an HTTP request ctx), cancelled the moment the
	// publisher returns; every DB query here would fail with "context
	// canceled". Use the daemon-lifetime ctx.
	// No agent run needs cutting here (决策 4-11): the gate that parks the
	// goal fired on the run that just completed, and per-goal serialization
	// means no other run is live — the review run enqueued below claims
	// within a dispatch tick.
	if err := d.maybeTriggerSquadReview(d.ctx, goalID); err != nil {
		logging.Infof("daemon: squad review trigger for %s: %v", goalID, err)
	}
	d.openReviewWindow(d.ctx, goalID)
}

// recoverReviewWindows re-opens the review window for every goal still in
// review — the startup face of Option B's Event≠Truth recovery: the ready
// publish, the fallback timers and the fired flags are in-memory, so a crash
// between a window's park and its ready publish would leave the human's card
// permanently unpatched. maybeFireReviewReady is idempotent (DB-derived).
func (d *Daemon) recoverReviewWindows(ctx context.Context) (int, error) {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id FROM goal WHERE status='review'`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		d.maybeFireReviewReady(ctx, id)
	}
	return len(ids), nil
}

// reviewReadyFallback bounds how long the human's approval card waits for
// the reviewer — a hung reviewer must not hold the human's decision hostage.
// A var (not const) so tests can shrink it.
var reviewReadyFallback = 10 * time.Minute

// openReviewWindow starts a NEW review window for the goal: the per-window
// card dedupe resets, and when no review runs are pending (no squad, no
// reviewers, or all excluded) the card fires immediately.
func (d *Daemon) openReviewWindow(ctx context.Context, goalID string) {
	d.mu.Lock()
	if d.reviewReadyFired == nil {
		d.reviewReadyFired = make(map[string]bool)
	}
	delete(d.reviewReadyFired, goalID) // a new park = a new window
	d.mu.Unlock()
	d.maybeFireReviewReady(ctx, goalID)
}

// maybeFireReviewReady publishes goal:review_ready when the goal is still in
// review, the human has not approved, and no review runs are pending. Called
// from openReviewWindow (park time), onRunTerminal (a review run finishing),
// and the fallback timer (a hung reviewer). The fired flag + timer stop make
// the card exactly-once per window.
func (d *Daemon) maybeFireReviewReady(ctx context.Context, goalID string) {
	var status, goalTitle string
	if err := d.st.DB().QueryRowContext(ctx, `SELECT status, title FROM goal WHERE id=?`, goalID).Scan(&status, &goalTitle); err != nil {
		return
	}
	if status != "review" {
		return // resolved/terminal — no card (the human already acted)
	}
	// An approve already on record means deliver is in flight (or done) —
	// the human decided without the card; don't send it post-hoc.
	var decided int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gate_decision WHERE goal_id=? AND decision='approve'`, goalID).Scan(&decided); err != nil {
		return
	}
	if decided > 0 {
		return
	}
	var pending int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review' AND status IN ('queued','running')`,
		goalID).Scan(&pending); err != nil {
		logging.Infof("daemon: review-ready pending check %s: %v", goalID, err)
		return
	}
	if pending > 0 {
		d.armReviewReadyFallback(goalID)
		return
	}
	d.publishReviewReady(goalID)
}

// armReviewReadyFallback schedules the one-shot fallback (once per window).
func (d *Daemon) armReviewReadyFallback(goalID string) {
	d.mu.Lock()
	if d.reviewReadyTimers == nil {
		d.reviewReadyTimers = make(map[string]*time.Timer)
	}
	if _, ok := d.reviewReadyTimers[goalID]; ok {
		d.mu.Unlock()
		return
	}
	t := time.AfterFunc(reviewReadyFallback, func() {
		d.mu.Lock()
		delete(d.reviewReadyTimers, goalID)
		d.mu.Unlock()
		d.publishReviewReady(goalID)
	})
	d.reviewReadyTimers[goalID] = t
	d.mu.Unlock()
}

// publishReviewReady fires the human's card trigger exactly once per window.
func (d *Daemon) publishReviewReady(goalID string) {
	d.mu.Lock()
	if d.reviewReadyFired[goalID] {
		d.mu.Unlock()
		return
	}
	d.reviewReadyFired[goalID] = true
	if t, ok := d.reviewReadyTimers[goalID]; ok {
		t.Stop()
		delete(d.reviewReadyTimers, goalID)
	}
	d.mu.Unlock()
	var goalTitle string
	_ = d.st.DB().QueryRowContext(context.Background(), `SELECT title FROM goal WHERE id=?`, goalID).Scan(&goalTitle)
	logging.Infof("daemon: review window ready for %q (%s) — notifying the human", goalTitle, goalID)
	// The review_duration anchor: the human's decision window STARTS here
	// (the reviewer's run time is not the human's decision time — the health
	// metric must measure the latter). An invisible activity row (the
	// timeline renders only its known action kinds).
	if _, err := d.st.DB().ExecContext(context.Background(),
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,'system','','review_ready','{}',?)`,
		uuid.NewString(), goalID, nowStr()); err != nil {
		logging.Infof("daemon: review-ready anchor for %s: %v", goalID, err)
	}
	d.bus.Publish(context.Background(), events.Event{Topic: "goal:review_ready", Payload: map[string]any{
		"goal_id": goalID,
	}})
}

// maybeTriggerSquadReview posts a system mention to each role="reviewer"
// member of the squad that OWNS the goal, and enqueues their review run —
// the platform writes the mention, going through the SAME mechanism as an
// agent writing one (the run row links to the comment via
// trigger_comment_id). The enqueue is called directly (not via
// CommentService.Create) because the goal is already in review — the review
// run must start DESPITE the review-state mention freeze (that freeze
// protects the branch under approval from NEW work; the review run IS the
// approval's evidence). Coalescing on (goal, reviewer) keeps repeated parks
// from stacking runs.
func (d *Daemon) maybeTriggerSquadReview(ctx context.Context, goalID string) error {
	var assigneeType, assigneeID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).
		Scan(&assigneeType, &assigneeID)
	if err == sql.ErrNoRows {
		return nil // goal vanished
	}
	if err != nil {
		return err
	}
	if assigneeType != "squad" || assigneeID == "" {
		return nil // not squad-owned — no squad rule applies
	}
	// A handoff_loop park has no code change to review — the human's call is
	// "continue or fail the collaboration", not the diff. The approval card
	// still fires (the park publishes goal:reviewing); the squad checkpoint
	// skips it.
	var reviewRequest string
	if err := d.st.DB().QueryRowContext(ctx, `SELECT review_request FROM goal WHERE id=?`, goalID).Scan(&reviewRequest); err != nil {
		return err
	}
	if strings.HasPrefix(reviewRequest, "handoff_loop:") {
		return nil
	}

	// The squad's leader (a reviewer who IS the leader would review its own
	// work — excluded, the review must come from a different member).
	var leaderID string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leaderID)

	// The squad's reviewers (members declared with role=reviewer). Collected
	// BEFORE the enqueues: each enqueue writes rows, and a query cursor held
	// open across writes breaks the single-connection in-memory test stores.
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT m.member_id, a.name FROM squad_member m
		 JOIN agent a ON a.id = m.member_id
		 WHERE m.squad_id=? AND m.member_type='agent' AND LOWER(TRIM(m.role))='reviewer'
		 ORDER BY m.created_at`, assigneeID)
	if err != nil {
		return err
	}
	var reviewers []struct{ id, name string }
	for rows.Next() {
		var r struct{ id, name string }
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close()
			return err
		}
		reviewers = append(reviewers, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range reviewers {
		if r.id == leaderID {
			continue // leader cannot review its own work
		}
		// The reviewer already has a pending REVIEW request on this goal — the
		// agent mentioned it itself (or a previous park did) — the request is
		// already out; a second comment would duplicate the ask. Scoped to
		// role='review' (P1-2, 决策 6-15⑧): under 决策 2-3 revised a HUMAN's
		// consult queued during the review window also sits on this agent as
		// a queued run — it must not suppress the platform's review request
		// (the consult is Claim-gated until release; the review run IS the
		// window's evidence and must fire).
		var pending int
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND role='review' AND status IN ('queued','running')`,
			goalID, r.id).Scan(&pending); err != nil {
			logging.Infof("daemon: squad review pending check %s → %s: %v", goalID, r.id, err)
			continue
		}
		if pending > 0 {
			continue
		}
		if err := d.enqueueSquadReview(ctx, goalID, r.id, r.name); err != nil {
			logging.Infof("daemon: squad review enqueue %s → %s: %v", goalID, r.id, err)
		}
	}
	return nil
}

// enqueueSquadReview writes the system review-request mention and enqueues
// the reviewer's run, linked by trigger_comment_id (the audit chain — the
// run row records WHICH comment caused it, same as an agent's mention).
func (d *Daemon) enqueueSquadReview(ctx context.Context, goalID, reviewerID, name string) error {
	ts := nowStr()
	commentID := uuid.NewString()
	content := "[@" + name + "](mention://agent/" + reviewerID + ") Please review the current changes (squad rule: after a member implements, a reviewer reviews). Give your opinion ONLY — do not modify any file."
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
		commentID, goalID, content, ts); err != nil {
		return err
	}
	if _, err := d.runSvc.EnqueueForMention(ctx, goalID, reviewerID, commentID); err != nil {
		return err
	}
	return nil
}

// onGoalFinished reacts to a goal reaching a terminal state. For CANCELLED
// goals (human Cancel, 决策 4-12), any still-running run is terminated —
// a cancelled goal must not keep an agent burning compute on work already
// decided dead. Done/failed goals have no live runs by construction (the
// terminal run just finished; per-goal serialization), so they are no-ops.
func (d *Daemon) onGoalFinished(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	status, _ := m["status"].(string)
	if status != "cancelled" {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	rows, err := d.st.DB().QueryContext(d.ctx,
		`SELECT id FROM run WHERE goal_id=? AND status='running'`, goalID)
	if err != nil {
		logging.Infof("daemon: cancel scan %s: %v", goalID, err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		logging.Infof("daemon: goal cancelled — stopping run %s", id)
		d.cancelRun(id, "stopped")
	}
}
