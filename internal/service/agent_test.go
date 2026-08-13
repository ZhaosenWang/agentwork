package service

import (
	"context"
	"testing"

	"github.com/eushing/agentwork/internal/events"
)

// TestDeleteAgentCancelsItsSubGoals: deleting an agent cancels the
// non-terminal sub-goals it executes or verifies — a sub_goal row has no FK
// on assignee/verifier, and a dangling id would make the next enqueue hit
// the run FK and stall the work item forever. Terminal history stays.
func TestDeleteAgentCancelsItsSubGoals(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	worker := seedAgent(t, st, "worker")
	verifier := seedAgent(t, st, "verifier")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sgRunning, err := gs.CreateSubGoal(ctx, g.ID, "work", "do it", worker, verifier, "agent", owner)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	sgDone, err := gs.CreateSubGoal(ctx, g.ID, "finished", "already verified", worker, "", "agent", owner)
	if err != nil {
		t.Fatalf("create second sub-goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='verified' WHERE id=?`, sgDone.ID); err != nil {
		t.Fatal(err)
	}

	// Deleting the worker cancels the running one and leaves verified history.
	bus := events.NewBus()
	as := NewAgentService(st, bus)
	if err := as.Delete(ctx, worker); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	sg1, _ := gs.GetSubGoal(ctx, sgRunning.ID)
	if sg1.Status != "cancelled" {
		t.Fatalf("running sub-goal must be cancelled with its assignee, got %q", sg1.Status)
	}
	sg2, _ := gs.GetSubGoal(ctx, sgDone.ID)
	if sg2.Status != "verified" {
		t.Fatalf("terminal sub-goal history must survive, got %q", sg2.Status)
	}

	// Deleting the VERIFIER likewise stops a sub-goal awaiting its verdict.
	sg3, err := gs.CreateSubGoal(ctx, g.ID, "pending verdict", "x", owner, verifier, "agent", owner)
	if err != nil {
		t.Fatalf("create verifier sub-goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='verifying' WHERE id=?`, sg3.ID); err != nil {
		t.Fatal(err)
	}
	if err := as.Delete(ctx, verifier); err != nil {
		t.Fatalf("delete verifier: %v", err)
	}
	sg3After, _ := gs.GetSubGoal(ctx, sg3.ID)
	if sg3After.Status != "cancelled" {
		t.Fatalf("verifying sub-goal must be cancelled with its verifier, got %q", sg3After.Status)
	}
}
