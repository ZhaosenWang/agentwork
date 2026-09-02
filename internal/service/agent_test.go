package service

import (
	"context"
	"testing"
	"time"

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


// TestSeedStewardForRuntime_Create covers the happy path: no steward exists,
// an active hwcloud runtime is present → SeedStewardForRuntime creates the
// steward agent bound to that exact runtime.
func TestSeedStewardForRuntime_Create(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	bus := events.NewBus()
	agentSvc := NewAgentService(st, bus)

	// Seed a machine + an active runtime named "hwcloud@laptop".
	if err := NewMachineService(st).Register(ctx, Machine{ID: "m1", Name: "laptop", Hostname: "h"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	rt, err := NewRuntimeService(st).Create(ctx, Runtime{Name: "hwcloud@laptop", MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	if err := agentSvc.SeedStewardForRuntime(ctx, "hwcloud@laptop"); err != nil {
		t.Fatalf("seed steward: %v", err)
	}

	steward, err := agentSvc.GetSteward(ctx)
	if err != nil {
		t.Fatalf("get steward after seed: %v", err)
	}
	if steward.Type != "steward" {
		t.Fatalf("steward type = %q, want steward", steward.Type)
	}
	if steward.RuntimeID != rt.ID {
		t.Fatalf("steward runtime_id = %q, want %q", steward.RuntimeID, rt.ID)
	}
	if steward.Name != "AI SHELL" {
		t.Fatalf("steward name = %q, want AI SHELL", steward.Name)
	}
}

// TestSeedStewardForRuntime_Idempotent covers the idempotent path: a steward
// already exists (even on a different runtime) → SeedStewardForRuntime is a
// no-op, the existing steward is unchanged.
func TestSeedStewardForRuntime_Idempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	bus := events.NewBus()
	agentSvc := NewAgentService(st, bus)

	// Seed a machine + two runtimes.
	if err := NewMachineService(st).Register(ctx, Machine{ID: "m1", Name: "laptop", Hostname: "h"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	rtClaude, err := NewRuntimeService(st).Create(ctx, Runtime{Name: "claude@laptop", MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed claude runtime: %v", err)
	}
	_, err = NewRuntimeService(st).Create(ctx, Runtime{Name: "hwcloud@laptop", MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed hwcloud runtime: %v", err)
	}

	// Pre-create a steward on the claude runtime.
	if err := agentSvc.SeedStewardForRuntime(ctx, "claude@laptop"); err != nil {
		t.Fatalf("pre-seed steward on claude: %v", err)
	}
	steward, err := agentSvc.GetSteward(ctx)
	if err != nil {
		t.Fatalf("get steward after pre-seed: %v", err)
	}
	originalRuntimeID := steward.RuntimeID

	// Now call with hwcloud — must be a no-op.
	if err := agentSvc.SeedStewardForRuntime(ctx, "hwcloud@laptop"); err != nil {
		t.Fatalf("second seed (should be no-op): %v", err)
	}
	steward2, err := agentSvc.GetSteward(ctx)
	if err != nil {
		t.Fatalf("get steward after second seed: %v", err)
	}
	if steward2.RuntimeID != originalRuntimeID {
		t.Fatalf("steward runtime changed: %q → %q (must be unchanged)", originalRuntimeID, steward2.RuntimeID)
	}
	if steward2.RuntimeID != rtClaude.ID {
		t.Fatalf("steward should still be on claude runtime, got %q", steward2.RuntimeID)
	}
}

// TestSeedStewardForRuntime_RuntimeNotFound covers the error path: the named
// runtime doesn't exist or isn't active → SeedStewardForRuntime returns an
// error, no steward is created.
func TestSeedStewardForRuntime_RuntimeNotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	agentSvc := NewAgentService(st, events.NewBus())

	// No runtime at all.
	err := agentSvc.SeedStewardForRuntime(ctx, "hwcloud@ghost")
	if err == nil {
		t.Fatal("expected error for non-existent runtime, got nil")
	}

	// Steward must not have been created.
	if _, err := agentSvc.GetSteward(ctx); err == nil {
		t.Fatal("steward should not exist after failed seed")
	}

	// Runtime exists but is absent (not active).
	if err := NewMachineService(st).Register(ctx, Machine{ID: "m1", Name: "laptop", Hostname: "h"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,machine_id,status,created_at) VALUES ('rt1','hwcloud@laptop','m1','absent',?)`,
		now()); err != nil {
		t.Fatalf("seed absent runtime: %v", err)
	}
	err = agentSvc.SeedStewardForRuntime(ctx, "hwcloud@laptop")
	if err == nil {
		t.Fatal("expected error for absent runtime, got nil")
	}
	if _, err := agentSvc.GetSteward(ctx); err == nil {
		t.Fatal("steward should not exist after failed seed on absent runtime")
	}
}

// TestSeedStewardForRuntime_PublishesAgentCreated verifies the bug fix: the
// steward is seeded AFTER daemon startup (when a machine registers via
// server.go seedStewardIfCLI), so recoverWorkers — which ran once at boot
// before the steward existed — will not build its worker. The only live-create
// signal is the agent:created event (same as the user-facing Create path); if
// insertSteward forgets to publish it, the steward's runs queue forever
// (dispatchOnce only claims for agents with a worker in d.workers).
func TestSeedStewardForRuntime_PublishesAgentCreated(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	bus := events.NewBus()
	agentSvc := NewAgentService(st, bus)

	// Capture agent:created events. Publish fans out in its own goroutine, so a
	// buffered channel + a brief wait keeps the assertion deterministic.
	got := make(chan Agent, 4)
	bus.Subscribe("agent:created", func(_ context.Context, e events.Event) {
		if a, ok := e.Payload.(Agent); ok {
			got <- a
		}
	})

	if err := NewMachineService(st).Register(ctx, Machine{ID: "m1", Name: "laptop", Hostname: "h"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	rt, err := NewRuntimeService(st).Create(ctx, Runtime{Name: "hwcloud@laptop", MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	if err := agentSvc.SeedStewardForRuntime(ctx, "hwcloud@laptop"); err != nil {
		t.Fatalf("seed steward: %v", err)
	}

	// The seed must have published exactly one agent:created carrying the
	// steward row (id + type + runtime binding) — the same contract as Create,
	// so onAgentCreated can build the worker.
	select {
	case a := <-got:
		if a.Type != "steward" {
			t.Fatalf("event agent type = %q, want steward", a.Type)
		}
		if a.Name != "AI SHELL" {
			t.Fatalf("event agent name = %q, want AI SHELL", a.Name)
		}
		if a.RuntimeID != rt.ID {
			t.Fatalf("event agent runtime_id = %q, want %q", a.RuntimeID, rt.ID)
		}
		if a.MaxConcurrent != 3 {
			t.Fatalf("event agent max_concurrent = %d, want 3", a.MaxConcurrent)
		}
		// The id must match the persisted row — onAgentCreated keys the worker
		// by this id, so a stale/mismatched id would build a worker nobody can
		// claim through.
		steward, err := agentSvc.GetSteward(ctx)
		if err != nil {
			t.Fatalf("get steward after seed: %v", err)
		}
		if a.ID != steward.ID {
			t.Fatalf("event agent id = %q, want persisted %q", a.ID, steward.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("agent:created not published after seed — steward worker will not be built")
	}

	// A second seed is an idempotent no-op (steward already exists): it must
	// NOT republish, or the daemon would rebuild a worker that already exists.
	if err := agentSvc.SeedStewardForRuntime(ctx, "hwcloud@laptop"); err != nil {
		t.Fatalf("second seed (should be no-op): %v", err)
	}
	select {
	case extra := <-got:
		t.Fatalf("idempotent seed republished agent:created for %q — should be silent", extra.ID)
	default:
	}
}
