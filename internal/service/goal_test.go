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

// TestAgentMentionTriggersHandoff: Agent A @mentions Agent B in a comment →
// goal assignee changes to B, a system comment is posted, and a run is enqueued for B.
func TestAgentMentionTriggersHandoff(t *testing.T) {
	_, _, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	gs := &GoalService{st: st, bus: events.NewBus()}
	rs := &RunService{st: st, bus: events.NewBus()}
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	cs.SetRunService(rs)
	cs.SetGoalService(gs)

	// Agent A owns the active goal
	g, err := gs.Create(ctx, Goal{Title: "handoff-test", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	_ = enqueueFirst(t, rs, g)

	// Agent A posts a comment @mentioning Agent B
	_, err = cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "[@B](mention://agent/" + agentB + ") please take over",
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}

	// Verify goal was reassigned to B
	after, _ := gs.Get(ctx, g.ID)
	if after.AssigneeID != agentB {
		t.Fatalf("goal should be reassigned to B, got %q", after.AssigneeID)
	}

	// Verify a system comment was posted
	comments, _ := cs.List(ctx, g.ID)
	found := false
	for _, c := range comments {
		if c.AuthorType == "system" && len(c.Content) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a system comment recording the handoff")
	}
}

// TestHumanMentionDoesNotHandoff: Human @mentions an agent → assignee unchanged, guest run only.
func TestHumanMentionDoesNotHandoff(t *testing.T) {
	_, _, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	gs := &GoalService{st: st, bus: events.NewBus()}
	rs := &RunService{st: st, bus: events.NewBus()}
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	cs.SetRunService(rs)
	cs.SetGoalService(gs)

	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	_ = enqueueFirst(t, rs, g)

	// Human posts a comment @mentioning Agent B
	_, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "",
		Content: "[@B](mention://agent/" + agentB + ") what does this return?",
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}

	after, _ := gs.Get(ctx, g.ID)
	if after.AssigneeID != agentA {
		t.Fatalf("human mention must not change assignee; expected A, got %q", after.AssigneeID)
	}
}

// TestSelfMentionDoesNotHandoff: Agent A @mentions A → no reassign, just coalesce.
func TestSelfMentionDoesNotHandoff(t *testing.T) {
	_, _, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	gs := &GoalService{st: st, bus: events.NewBus()}
	rs := &RunService{st: st, bus: events.NewBus()}
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	cs.SetRunService(rs)
	cs.SetGoalService(gs)

	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	_ = enqueueFirst(t, rs, g)

	_, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: agentA,
		Content: "[@me](mention://agent/" + agentA + ") note to self",
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}

	after, _ := gs.Get(ctx, g.ID)
	if after.AssigneeID != agentA {
		t.Fatalf("self-mention must not change assignee; expected A, got %q", after.AssigneeID)
	}
}

// TestMarkDoneByAssignee: Current assignee marks goal done → status=done + system comment.
func TestMarkDoneByAssignee(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	_ = enqueueFirst(t, rs, g)

	if err := gs.MarkDone(ctx, g.ID, agentA, "all done"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("expected done, got %q", after.Status)
	}
}

// TestMarkDoneByNonAssignee: Non-assignee tries → error.
func TestMarkDoneByNonAssignee(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	_ = enqueueFirst(t, rs, g)

	if err := gs.MarkDone(ctx, g.ID, agentB, "not mine"); err == nil {
		t.Fatal("expected error when non-assignee marks done")
	}
}

// Ensure the test binary doesn't time out on the background bus goroutines.
var _ = time.Second