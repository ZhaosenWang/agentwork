package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// newStewardWorkerDaemon builds a minimal Daemon with the worker surfaces the
// dispatch path needs: an initialized workers map + a live ctx. It mirrors the
// wiring daemon.New does (subscribe agent:created → onAgentCreated) without
// starting the dispatch loop, so a test can assert worker state directly.
func newStewardWorkerDaemon(t *testing.T) (*Daemon, *store.Store, *service.AgentService) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,created_at) VALUES ('rt1','rt1',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	agentSvc := service.NewAgentService(st, bus)
	d := &Daemon{
		st:          st,
		bus:         bus,
		agentSvc:    agentSvc,
		runSvc:      service.NewRunService(st, bus),
		workers:     make(map[string]*agentWorker),
	}
	ctxCancel := func() {}
	d.ctx, ctxCancel = context.WithCancel(context.Background())
	t.Cleanup(ctxCancel)
	bus.Subscribe("agent:created", d.onAgentCreated)
	return d, st, agentSvc
}

// TestOnAgentCreated_BuildsStewardWorker is the daemon-side lock on the bug
// fix: seeding the steward after daemon startup (the machine-register path
// server.go seedStewardIfCLI) must build its per-agent worker, or the steward's
// runs queue forever — dispatchOnce only claims for agents with a worker in
// d.workers (the Claim SQL's IN-list is the ready-agent set, and a steward
// with no worker is never ready). recoverWorkers ran once at boot before the
// steward existed, so the agent:created event is the only live signal.
func TestOnAgentCreated_BuildsStewardWorker(t *testing.T) {
	d, st, agentSvc := newStewardWorkerDaemon(t)
	ctx := context.Background()

	// A machine-owned active runtime is what SeedStewardForRuntime binds to.
	if err := service.NewMachineService(st).Register(ctx, service.Machine{ID: "m1", Name: "laptop", Hostname: "h"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "hwcloud@laptop", MachineID: "m1"}); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Seed AFTER the daemon is wired — the real machine-register path. This
	// publishes agent:created, which onAgentCreated must turn into a worker.
	if err := agentSvc.SeedStewardForRuntime(ctx, "hwcloud@laptop"); err != nil {
		t.Fatalf("seed steward: %v", err)
	}
	steward, err := agentSvc.GetSteward(ctx)
	if err != nil {
		t.Fatalf("get steward: %v", err)
	}

	// Publish is async (each handler in its own goroutine); poll briefly for the
	// worker to appear — the handler does no I/O beyond a map write + goroutine
	// start, so a second is a generous bound for a wedged handler to fail.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		_, ok := d.workers[steward.ID]
		d.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.mu.Lock()
	w, ok := d.workers[steward.ID]
	d.mu.Unlock()
	if !ok {
		t.Fatalf("steward %s has no worker after agent:created — runs will queue forever", steward.ID)
	}
	if w.maxConc != 3 {
		t.Fatalf("steward worker maxConc = %d, want 3", w.maxConc)
	}
}

// TestRecoverWorkers_CoversSteward is the startup-fallback lock: a steward
// that existed before the daemon booted (seeded during the same startup, ahead
// of daemon.New) is covered by recoverWorkers' full-table scan, not the event.
// The two paths together guarantee a steward always lands a worker regardless
// of when it was seeded relative to daemon boot.
func TestRecoverWorkers_CoversSteward(t *testing.T) {
	d, st, agentSvc := newStewardWorkerDaemon(t)
	ctx := context.Background()

	if err := service.NewMachineService(st).Register(ctx, service.Machine{ID: "m1", Name: "laptop", Hostname: "h"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	if _, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "hwcloud@laptop", MachineID: "m1"}); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	// Seed before recoverWorkers, mirroring startup order (SeedSteward runs
	// ahead of daemon.New; the event fires before onAgentCreated subscribes).
	if err := agentSvc.SeedSteward(ctx); err != nil {
		t.Fatalf("seed steward: %v", err)
	}
	steward, err := agentSvc.GetSteward(ctx)
	if err != nil {
		t.Fatalf("get steward: %v", err)
	}

	d.recoverWorkers(ctx)

	d.mu.Lock()
	_, ok := d.workers[steward.ID]
	d.mu.Unlock()
	if !ok {
		t.Fatalf("recoverWorkers did not build a worker for pre-existing steward %s", steward.ID)
	}
}
