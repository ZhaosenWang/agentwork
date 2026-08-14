package service

import (
	"context"
	"testing"
)

// TestFinishStampsReconciledAt: the normal Finish path reconciles AND stamps
// run.reconciled_at in the same transaction — a completed run is never left
// terminal-but-unreconciled (P0-1, 决策 6-11).
func TestFinishStampsReconciledAt(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var reconciledAt string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT reconciled_at FROM run WHERE id=?`, r.ID).Scan(&reconciledAt); err != nil {
		t.Fatalf("load reconciled_at: %v", err)
	}
	if reconciledAt == "" {
		t.Fatal("Finish must stamp reconciled_at in the reconcile transaction")
	}
}

// TestReconcilePendingTerminalReplays: the crash window — the run row went
// terminal but the daemon died before the reconcile. The startup replay
// advances the sub-goal, materializes the Change, lands the report comment
// ONCE, and stamps reconciled_at; a second replay is a no-op (no duplicate
// change, no duplicate comment).
func TestReconcilePendingTerminalReplays(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	b := seedAgent(t, st, "worker")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var sgRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&sgRun); err != nil {
		t.Fatalf("sub-goal run: %v", err)
	}
	// Simulate the daemon's run-end stamps + crash BEFORE Finish's reconcile:
	// the run row is terminal, reconciled_at is ''.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', result_summary='implemented', base_ref='b1', head_ref='h1', finished_at=? WHERE id=?`,
		now(), sgRun); err != nil {
		t.Fatal(err)
	}

	n, err := rs.ReconcilePendingTerminal(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 replayed run, got %d", n)
	}
	after, _ := gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "verified" {
		t.Fatalf("replay must verify the sub-goal, got %q", after.Status)
	}
	var changes, comments int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change WHERE goal_id=?`, g.ID).Scan(&changes); err != nil || changes != 1 {
		t.Fatalf("replay must materialize exactly 1 change, got %d (err %v)", changes, err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE run_id=?`, sgRun).Scan(&comments); err != nil || comments != 1 {
		t.Fatalf("replay must land the report comment once, got %d (err %v)", comments, err)
	}
	var reconciledAt string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT reconciled_at FROM run WHERE id=?`, sgRun).Scan(&reconciledAt); err != nil || reconciledAt == "" {
		t.Fatalf("replay must stamp reconciled_at, got %q (err %v)", reconciledAt, err)
	}

	// Second replay: nothing new.
	n, err = rs.ReconcilePendingTerminal(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second replay must find nothing, got n=%d err=%v", n, err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change WHERE goal_id=?`, g.ID).Scan(&changes); err != nil || changes != 1 {
		t.Fatalf("second replay must not duplicate changes, got %d (err %v)", changes, err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE run_id=?`, sgRun).Scan(&comments); err != nil || comments != 1 {
		t.Fatalf("second replay must not duplicate the report, got %d (err %v)", comments, err)
	}
}

// TestReconcilePendingTerminalReplaysOwnerRun: the same crash window on the
// goal level — an owner run went terminal before the reconcile; the replay
// promotes the goal to done.
func TestReconcilePendingTerminalReplaysOwnerRun(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', result_summary='done', finished_at=? WHERE id=?`,
		now(), r.ID); err != nil {
		t.Fatal(err)
	}
	n, err := rs.ReconcilePendingTerminal(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 replayed run, got %d", n)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("replay must promote the goal, got %q", after.Status)
	}
}

// TestReconcilePendingTerminalSkipsUnstartedCancelled: a run cancelled while
// still QUEUED never executed — it has no reconcile semantics, and replaying
// it would bump execution_attempt and enqueue a retry on a cancelled
// sub-goal. The scan must skip it (started_at=”).
func TestReconcilePendingTerminalSkipsUnstartedCancelled(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	b := seedAgent(t, st, "worker")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// Cancel: the queued run → cancelled (never started), sub-goal → cancelled.
	if _, err := gs.CancelSubGoal(ctx, sg.ID); err != nil {
		t.Fatalf("cancel sub-goal: %v", err)
	}
	var attemptBefore, queuedBefore int
	_ = st.DB().QueryRowContext(ctx, `SELECT execution_attempt FROM sub_goal WHERE id=?`, sg.ID).Scan(&attemptBefore)
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE sub_goal_id=? AND status IN ('queued','running')`, sg.ID).Scan(&queuedBefore)

	n, err := rs.ReconcilePendingTerminal(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 0 {
		t.Fatalf("unstarted cancelled runs must be skipped, got %d replayed", n)
	}
	var attemptAfter, queuedAfter int
	_ = st.DB().QueryRowContext(ctx, `SELECT execution_attempt FROM sub_goal WHERE id=?`, sg.ID).Scan(&attemptAfter)
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE sub_goal_id=? AND status IN ('queued','running')`, sg.ID).Scan(&queuedAfter)
	if attemptAfter != attemptBefore || queuedAfter != queuedBefore {
		t.Fatalf("replay must not touch a cancelled sub-goal (attempt %d→%d, queued %d→%d)",
			attemptBefore, attemptAfter, queuedBefore, queuedAfter)
	}
}
