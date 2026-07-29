package service

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// newTestStore opens a fresh in-memory SQLite store for a test. Each test gets
// its own DB so they're independent. (modernc.org/sqlite supports ":memory:".)
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newTestCluster wires Goal+Run+Comment with their cross-references, plus a
// minimal runtime+agent fixture so goal enqueue has a real agent to target.
func newTestCluster(t *testing.T) (*GoalService, *RunService, *CommentService, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	bus := events.NewBus()
	gs := NewGoalService(st, bus)
	rs := NewRunService(st, bus)
	cs := NewCommentService(st, bus)
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	cs.SetRunService(rs)
	cs.SetGoalService(gs)
	return gs, rs, cs, st
}

// seedAgent inserts a runtime + agent and returns the agent id.
func seedAgent(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntimeService(st).Create(ctx, Runtime{Name: "rt-" + name, Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	a, err := NewAgentService(st, events.NewBus()).Create(ctx, Agent{Name: name, RuntimeID: rt.ID, MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return a.ID
}

// enqueueFirst forces the first run for an already-created active goal and
// returns it. (The HTTP handler does this; tests call the service directly.)
func enqueueFirst(t *testing.T, rs *RunService, g *Goal) *Run {
	t.Helper()
	r, err := rs.EnqueueForGoal(context.Background(), *g)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return r
}

// TestReconcileNormalCompletion: A run completes while A is still the owner →
// goal flips to done. The happy path that proves reconcile DOES advance the
// goal when the run belongs to the current assignee.
func TestReconcileNormalCompletion(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	g, err := gs.Create(ctx, Goal{Title: "do thing", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)

	// Simulate the daemon finishing a successful run.
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("expected goal done, got %q", after.Status)
	}
}

// TestHandoffDoesNotClobber: A's run completes AFTER the goal was handed off
// to B. A's result must NOT flip the goal to done — A is no longer the owner,
// so reconcile discards it. This is the core self-consistency invariant
// (DESIGN.zh.md §5.1/§7): the design without an external authority.
func TestHandoffDoesNotClobber(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	rA := enqueueFirst(t, rs, g)
	// Claim A's run so it's "running" (an in-flight run).
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatalf("claim A: %v", err)
	}

	// Hand off to B while A's run is in flight. This enqueues a B run.
	if _, err := gs.Assign(ctx, g.ID, "agent", agentB, "handoff note"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := rs.EnqueueForGoal(ctx, Goal{ID: g.ID, AssigneeType: "agent", AssigneeID: agentB}); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}

	// A's in-flight run now reports completion. It must NOT change goal status.
	if err := rs.Finish(ctx, rA.ID, "completed", "A finished"); err != nil {
		t.Fatalf("finish A: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status == "done" {
		t.Fatalf("A's orphaned run must not mark goal done; status=%q", after.Status)
	}
	if after.AssigneeID != agentB {
		t.Fatalf("goal should still be assigned to B; got %q", after.AssigneeID)
	}
	// The handoff note must survive A's orphaned completion — A no longer owns
	// the goal, so its run must not clear the note meant for B. (P2 regression.)
	if after.HandoffNote != "handoff note" {
		t.Fatalf("handoff note must be preserved across A's orphaned finish; got %q", after.HandoffNote)
	}
}

// TestHandoffNoteClearedOnOwnerDone: when the owning run completes, the
// handoff/wakeup note is cleared (consumed) as part of the same reconcile that
// promotes the goal to done. Confirms handoff_note cleanup lives in the goal
// layer, gated on owns+done — not in the daemon.
func TestHandoffNoteClearedOnOwnerDone(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	// Pre-seed a handoff note directly (Assign takes one, or a child wake sets it).
	g, err := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active", HandoffNote: "scope note"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	mid, _ := gs.Get(ctx, g.ID)
	if mid.HandoffNote != "scope note" {
		t.Fatalf("note should be present at start; got %q", mid.HandoffNote)
	}
	// Owner A completes the run legitimately → goal done + note consumed.
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("expected done, got %q", after.Status)
	}
	if after.HandoffNote != "" {
		t.Fatalf("handoff note must be cleared on owner-done; got %q", after.HandoffNote)
	}
}

// TestCancelNotClobbered: a goal is cancelled while a run is in flight. The
// run completing must not flip the goal out of cancelled.
func TestCancelNotClobbered(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	r := enqueueFirst(t, rs, g)
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := gs.Cancel(ctx, g.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The in-flight run reports completion.
	if err := rs.Finish(ctx, r.ID, "completed", "late"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %q (in-flight run clobbered)", after.Status)
	}
}

// TestSubGoalCoordination: parent waits on children; when all children finish,
// the parent is woken (re-queued) with a wakeup note. Proves the dynamic
// wait-set + child→parent notification path.
func TestSubGoalCoordination(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	parent, _ := gs.Create(ctx, Goal{Title: "parent", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	pr := enqueueFirst(t, rs, parent)

	// Parent's first run creates two children and then waits on them.
	c1, _ := gs.Create(ctx, Goal{Title: "child1", AssigneeType: "agent", AssigneeID: agentA, Status: "active", ParentID: parent.ID})
	c2, _ := gs.Create(ctx, Goal{Title: "child2", AssigneeType: "agent", AssigneeID: agentA, Status: "active", ParentID: parent.ID})
	_ = enqueueFirst(t, rs, c1)
	_ = enqueueFirst(t, rs, c2)
	if err := gs.WaitChildren(ctx, parent.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// The parent's first run ends (it called wait). It should NOT advance the
	// goal — the goal is blocked.
	if err := rs.Finish(ctx, pr.ID, "completed", "parent spawned children"); err != nil {
		t.Fatalf("finish parent run1: %v", err)
	}
	pAfter, _ := gs.Get(ctx, parent.ID)
	if pAfter.Status != "blocked" {
		t.Fatalf("parent should be blocked, got %q", pAfter.Status)
	}

	// Children complete. The LAST child's completion should wake the parent.
	rc1 := enqueueFirst(t, rs, c1)
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := rs.Finish(ctx, rc1.ID, "completed", "c1 done"); err != nil {
		t.Fatalf("finish c1: %v", err)
	}
	// c2 still inflight → parent should NOT wake yet.
	pAfter, _ = gs.Get(ctx, parent.ID)
	if pAfter.Status != "blocked" {
		t.Fatalf("parent should still be blocked (c2 inflight), got %q", pAfter.Status)
	}

	rc2 := enqueueFirst(t, rs, c2)
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatalf("claim c2: %v", err)
	}
	if err := rs.Finish(ctx, rc2.ID, "completed", "c2 done"); err != nil {
		t.Fatalf("finish c2: %v", err)
	}

	// Now all children terminal → parent woken: status active + a new run queued.
	pAfter, _ = gs.Get(ctx, parent.ID)
	if pAfter.Status != "active" {
		t.Fatalf("parent should be woken to active, got %q", pAfter.Status)
	}
	runs, _ := rs.List(ctx, parent.ID)
	if len(runs) < 2 {
		t.Fatalf("parent should have a second (wakeup) run; got %d runs", len(runs))
	}
	if pAfter.HandoffNote == "" {
		t.Fatalf("parent should carry a wakeup note")
	}
}

// TestCoalescePending: enqueuing a second run for the same (goal,agent) while
// one is pending coalesces — exactly one queued/running run per pair.
func TestCoalescePending(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	r1 := enqueueFirst(t, rs, g)
	// Hand off to A again (re-enqueue) while r1 is still queued.
	if _, err := gs.Assign(ctx, g.ID, "agent", agentA, "again"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := rs.EnqueueForGoal(ctx, Goal{ID: g.ID, AssigneeType: "agent", AssigneeID: agentA}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	pending := 0
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "running" {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("expected exactly 1 pending run after coalesce, got %d (r1=%s)", pending, r1.Status)
	}
}

// Ensure the test binary doesn't time out on the background bus goroutines.
var _ = time.Second