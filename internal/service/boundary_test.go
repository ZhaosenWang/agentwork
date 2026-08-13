package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCreateSubGoalRejectsNonActive (P1-1): sub-goals hang only off ACTIVE
// goals — review is an execution freeze point and terminal goals have no
// execution left to split.
func TestCreateSubGoalRejectsNonActive(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	b := seedAgent(t, st, "worker")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	for _, status := range []string{"done", "review", "failed", "cancelled", "backlog"} {
		if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status=? WHERE id=?`, status, g.ID); err != nil {
			t.Fatal(err)
		}
		_, err := gs.CreateSubGoal(ctx, g.ID, "work", "sub", b, "", "agent", a)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s goal must reject sub-goal creation, got %v", status, err)
		}
	}
	// Back to active: allowed.
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='active' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.CreateSubGoal(ctx, g.ID, "work", "sub", b, "", "agent", a); err != nil {
		t.Fatalf("active goal must allow sub-goal creation, got %v", err)
	}
}

// TestVerifySubGoalVerifierIdentity (P1-2): the verdict authority is the
// NAMED verifier — a verify-role run whose agent is not the sub-goal's
// verifier cannot judge, and a verify run that is no longer running has no
// verdict window.
func TestVerifySubGoalVerifierIdentity(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	b := seedAgent(t, st, "worker")
	qa := seedAgent(t, st, "qa")
	intruder := seedAgent(t, st, "intruder")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work", "sub", b, qa, "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var assigneeRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&assigneeRun); err != nil {
		t.Fatalf("assignee run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, assigneeRun); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, assigneeRun, "completed", "implemented"); err != nil {
		t.Fatalf("finish assignee: %v", err)
	}
	var verifyRunID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='verify' LIMIT 1`, sg.ID).Scan(&verifyRunID); err != nil {
		t.Fatalf("verify run must be enqueued: %v", err)
	}

	// The verify run is live (claimed by the daemon — the tool is only
	// reachable while the run's MCP executor exists).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running' WHERE id=?`, verifyRunID); err != nil {
		t.Fatal(err)
	}
	// An intruder agent hijacks the verify run (agent_id tampered) — rejected.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET agent_id=? WHERE id=?`, intruder, verifyRunID); err != nil {
		t.Fatal(err)
	}
	err = gs.VerifySubGoal(ctx, verifyRunID, "passed", "ok", "")
	if err == nil || !strings.Contains(err.Error(), "verifier agent") {
		t.Fatalf("non-verifier agent must be rejected, got %v", err)
	}

	// The real verifier's run, but already finished — no verdict window.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET agent_id=?, status='completed' WHERE id=?`, qa, verifyRunID); err != nil {
		t.Fatal(err)
	}
	err = gs.VerifySubGoal(ctx, verifyRunID, "passed", "ok", "")
	if err == nil || !strings.Contains(err.Error(), "no longer running") {
		t.Fatalf("finished verify run must be rejected, got %v", err)
	}
}

// TestConflictDoesNotArmAttention (P1-3): a conflicted change is the
// assignee's rework, not the owner's work — attention stays empty through
// the conflict window (no owner spawn), and re-arms only when the rework
// returns the change to ready.
func TestConflictDoesNotArmAttention(t *testing.T) {
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
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, sgRun); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, sgRun, "completed", "implemented"); err != nil {
		t.Fatalf("finish sub-goal run: %v", err)
	}
	// Ready change → attention armed + owner spawned.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var changeID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM change WHERE goal_id=? AND status='ready' LIMIT 1`, g.ID).Scan(&changeID); err != nil {
		t.Fatalf("ready change: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Attention != "integration" {
		t.Fatalf("ready change must arm attention, got %q", g1.Attention)
	}
	var ownerRuns int
	_ = st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='owner'`, g.ID).Scan(&ownerRuns)

	// Integration conflicts → the change is the assignee's rework now.
	if err := gs.MarkChangeIntegrating(ctx, changeID); err != nil {
		t.Fatal(err)
	}
	if err := gs.MarkChangeIntegrated(ctx, changeID, false); err != nil {
		t.Fatal(err)
	}
	// The change.conflict latch edge fires a reconcile.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile after conflict: %v", err)
	}
	g2, _ := gs.Get(ctx, g.ID)
	if g2.Attention != "" {
		t.Fatalf("conflict must NOT arm owner attention, got %q", g2.Attention)
	}
	var ownerRunsAfter int
	_ = st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='owner'`, g.ID).Scan(&ownerRunsAfter)
	if ownerRunsAfter != ownerRuns {
		t.Fatalf("conflict must not spawn the owner (%d→%d owner runs)", ownerRuns, ownerRunsAfter)
	}

	// The assignee reworks → new revision → ready again → attention re-arms.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO change_revision (id,change_id,seq,base_ref,head_ref,created_at) VALUES (?,?,2,'b2','h2',?)`,
		newID(), changeID, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE change SET status='ready' WHERE id=?`, changeID); err != nil {
		t.Fatal(err)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile after rework: %v", err)
	}
	g3, _ := gs.Get(ctx, g.ID)
	if g3.Attention != "integration" {
		t.Fatalf("reworked ready change must re-arm attention, got %q", g3.Attention)
	}
}

// TestAttentionNotArmedForNonActiveGoals: a ready change on a terminal goal
// must not arm owner attention — there is no owner to wake. (The pre-guard
// era left exactly such rows: a done goal with a ready change showed
// "待集成变更" forever.)
func TestAttentionNotArmedForNonActiveGoals(t *testing.T) {
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
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, sgRun); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, sgRun, "completed", "implemented"); err != nil {
		t.Fatalf("finish sub-goal run: %v", err)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Attention != "integration" {
		t.Fatalf("active goal with a ready change must arm attention, got %q", g1.Attention)
	}

	// The goal reaches done (deliver path) while the change is still ready.
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile after done: %v", err)
	}
	g2, _ := gs.Get(ctx, g.ID)
	if g2.Attention != "" {
		t.Fatalf("terminal goals must not carry attention, got %q", g2.Attention)
	}
}

// TestReconcileAllActiveReArmsAttention (P0-3): a crash can lose the latch
// events AFTER their transactions committed — the DB has a ready change but
// attention was never derived and no owner was spawned. The startup sweep
// re-derives from DB truth (idempotent ReconcileGoal per active goal) and
// re-spawns exactly what the state demands. Terminal goals are untouched.
func TestReconcileAllActiveReArmsAttention(t *testing.T) {
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
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, sgRun); err != nil {
		t.Fatal(err)
	}
	// The sub-goal verifies + the change becomes ready — but the Coordinator
	// never runs (simulated lost events): attention stays '' and no owner
	// run exists beyond the initial... the goal never had an owner run here.
	if err := rs.Finish(ctx, sgRun, "completed", "implemented"); err != nil {
		t.Fatalf("finish sub-goal run: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Attention != "" {
		t.Fatalf("attention must be empty before any reconcile, got %q", g1.Attention)
	}

	// The startup sweep re-arms from DB truth.
	n, err := gs.ReconcileAllActive(ctx)
	if err != nil {
		t.Fatalf("reconcile all: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 active goal reconciled, got %d", n)
	}
	g2, _ := gs.Get(ctx, g.ID)
	if g2.Attention != "integration" {
		t.Fatalf("sweep must re-arm attention from the ready change, got %q", g2.Attention)
	}
	var ownerRuns int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='owner' AND status IN ('queued','running')`, g.ID).Scan(&ownerRuns); err != nil {
		t.Fatal(err)
	}
	if ownerRuns != 1 {
		t.Fatalf("sweep must spawn the owner run, got %d pending owner runs", ownerRuns)
	}

	// Terminal goals are untouched.
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET attention='' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	n, err = gs.ReconcileAllActive(ctx)
	if err != nil || n != 0 {
		t.Fatalf("no active goals left — sweep must be empty, got n=%d err=%v", n, err)
	}
}

// TestActivateBacklogGoal (决策 6-14): backlog → active is the missing
// entry back into execution — conditional on backlog only, and the spawn
// goes through the unified owner-run entry (P0.5), so a human-assigned
// goal activates without a run, exactly like creation.
func TestActivateBacklogGoal(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "worker")
	domID := seedDomain(t, st)

	// A backlog goal with an agent assignee: no run at creation.
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "backlog", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if runs, _ := rs.List(ctx, g.ID); len(runs) != 0 {
		t.Fatalf("backlog goal must not enqueue at creation, got %d runs", len(runs))
	}
	after, err := gs.Activate(ctx, g.ID)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if after.Status != "active" {
		t.Fatalf("activate must move backlog → active, got %q", after.Status)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 1 || runs[0].Role != "owner" {
		t.Fatalf("activate must spawn the owner run, got %+v", runs)
	}

	// Conditional: a second activate on a non-backlog goal is refused.
	if _, err := gs.Activate(ctx, g.ID); !errors.Is(err, ErrValidation) {
		t.Fatalf("activating an active goal must be rejected, got %v", err)
	}
	// Terminal goals too.
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.Activate(ctx, g.ID); !errors.Is(err, ErrValidation) {
		t.Fatalf("activating a done goal must be rejected, got %v", err)
	}

	// Human-assigned backlog: activates without a run (manual placeholder).
	g2, err := gs.Create(ctx, Goal{Title: "human work", AssigneeType: "human", Status: "backlog"})
	if err != nil {
		t.Fatalf("create human goal: %v", err)
	}
	after2, err := gs.Activate(ctx, g2.ID)
	if err != nil {
		t.Fatalf("activate human goal: %v", err)
	}
	if after2.Status != "active" {
		t.Fatalf("human goal must activate, got %q", after2.Status)
	}
	if runs, _ := rs.List(ctx, g2.ID); len(runs) != 0 {
		t.Fatalf("human-assigned activation must not enqueue, got %d runs", len(runs))
	}
}

// TestCreateUnassignedBacklogGoal: a title-only goal (the web form's
// "无（进入 backlog）" option) is a valid unassigned work item — the
// backend must NOT default it to an agent goal with an empty id (the 400
// "assignee_id is required for an agent goal" a bare title used to hit).
// It lands as a human placeholder, backlog, no run.
func TestCreateUnassignedBacklogGoal(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	g, err := gs.Create(ctx, Goal{Title: "自我介绍一下啊"})
	if err != nil {
		t.Fatalf("title-only goal must be creatable: %v", err)
	}
	if g.AssigneeType != "human" || g.AssigneeID != "" {
		t.Fatalf("unassigned goal must be a human placeholder, got %s/%s", g.AssigneeType, g.AssigneeID)
	}
	if g.Status != "backlog" {
		t.Fatalf("unassigned goal must stay backlog, got %q", g.Status)
	}
	if runs, _ := rs.List(ctx, g.ID); len(runs) != 0 {
		t.Fatalf("unassigned goal must not enqueue, got %d runs", len(runs))
	}
	// A bare id without a type still defaults to agent (API convenience).
	a := seedAgent(t, st, "a")
	g2, err := gs.Create(ctx, Goal{Title: "bare id", AssigneeID: a, Status: "backlog", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatalf("bare id must default to agent: %v", err)
	}
	if g2.AssigneeType != "agent" || g2.AssigneeID != a {
		t.Fatalf("bare id must default to agent, got %s/%s", g2.AssigneeType, g2.AssigneeID)
	}
}

// TestRecoverStuckRunningSkipsTerminalGoals (P0-4): a run left 'running' on
// a goal the human CANCELLED while the daemon was down must not be
// resurrected by the restart — it is stamped cancelled instead of burning
// compute on already-decided work. Active-goal stuck runs still requeue.
func TestRecoverStuckRunningSkipsTerminalGoals(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)

	// Goal cancelled while its run was in flight (the daemon died before
	// killing it — Cancel only drops queued runs; running ones are the
	// daemon's to cut).
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if _, err := rs.Claim(ctx, []string{a}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := gs.Cancel(ctx, g.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	n, err := rs.RecoverStuckRunning(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 0 {
		t.Fatalf("terminal-goal run must not be requeued, got %d", n)
	}
	var status, reason string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status, cancel_reason FROM run WHERE id=?`, r.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || reason != "goal_terminal" {
		t.Fatalf("terminal-goal run must be stamped cancelled/goal_terminal, got %s/%s", status, reason)
	}

	// An active goal's stuck run still requeues normally.
	g2, err := gs.Create(ctx, Goal{Title: "g2", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal 2: %v", err)
	}
	r2 := enqueueFirst(t, rs, g2)
	if _, err := rs.Claim(ctx, []string{a}); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	n, err = rs.RecoverStuckRunning(ctx)
	if err != nil || n != 1 {
		t.Fatalf("active-goal stuck run must requeue, got n=%d err=%v", n, err)
	}
	var status2 string
	_ = st.DB().QueryRowContext(ctx, `SELECT status FROM run WHERE id=?`, r2.ID).Scan(&status2)
	if status2 != "queued" {
		t.Fatalf("active-goal run must be back to queued, got %q", status2)
	}
}
