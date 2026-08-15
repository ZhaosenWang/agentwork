package service

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/link"
)

// TestMachineRegistry covers the /connect registry (CLI 分支 Phase 1):
// register upserts across reconnects, heartbeats re-mark online, the stale
// sweep flips silent machines offline, and probe updates land.
func TestMachineRegistry(t *testing.T) {
	st := newTestStore(t)
	svc := NewMachineService(st)
	ctx := context.Background()

	// Fresh register.
	if err := svc.Register(ctx, Machine{ID: "m1", Name: "dev", Hostname: "host1", Version: "0.1.0"}, `[{"name":"claude","version":"2.0"}]`); err != nil {
		t.Fatalf("register: %v", err)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 machine, got %d (err %v)", len(list), err)
	}
	if list[0].Status != "connected" || list[0].ProbedCLIs == "[]" {
		t.Fatalf("expected connected + probe report, got %+v", list[0])
	}

	// Re-register (reconnect) upserts the same row.
	if err := svc.Register(ctx, Machine{ID: "m1", Name: "dev2", Hostname: "host1"}, "[]"); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	list, _ = svc.List(ctx)
	if len(list) != 1 || list[0].Name != "dev2" {
		t.Fatalf("re-register must upsert, got %+v", list)
	}

	// A DIFFERENT machine must not take an existing name: runtime rows are
	// keyed "<cli>@<name>", so a silent takeover would reroute the first
	// machine's runs.
	if err := svc.Register(ctx, Machine{ID: "m2", Name: "dev2", Hostname: "host2"}, "[]"); err == nil {
		t.Fatalf("duplicate machine name must be rejected")
	}
	// The same machine may keep its own name on re-register (no-op check).
	if err := svc.Register(ctx, Machine{ID: "m1", Name: "dev2", Hostname: "host1"}, "[]"); err != nil {
		t.Fatalf("same machine re-register under its own name: %v", err)
	}

	// A FRESH machine must NOT be swept (the timezone-comparison regression:
	// a local-zone cutoff made every connected machine look stale and the
	// sweep flapped offline every tick while heartbeats re-marked it).
	if n, err := svc.MarkStale(ctx, time.Now().UTC().Add(-90*time.Second)); err != nil || n != 0 {
		t.Fatalf("fresh machine must survive the sweep: n=%d err=%v", n, err)
	}

	// Backdate the row (heartbeats stopped an hour ago), then sweep.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE machine SET last_seen_at=? WHERE id='m1'`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if n, err := svc.MarkStale(ctx, time.Now().UTC().Add(-90*time.Second)); err != nil || n != 1 {
		t.Fatalf("stale sweep: n=%d err=%v", n, err)
	}
	list, _ = svc.List(ctx)
	if list[0].Status != "offline" {
		t.Fatalf("expected offline after stale sweep, got %+v", list[0])
	}

	// Heartbeat brings it back online.
	if err := svc.Heartbeat(ctx, "m1"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	list, _ = svc.List(ctx)
	if list[0].Status != "connected" {
		t.Fatalf("heartbeat must re-mark connected, got %+v", list[0])
	}

	// Probe update lands.
	if err := svc.UpdateProbe(ctx, "m1", `[{"name":"opencode","version":"0.2"}]`); err != nil {
		t.Fatalf("probe update: %v", err)
	}
	list, _ = svc.List(ctx)
	if list[0].ProbedCLIs == `[{"name":"claude","version":"2.0"}]` {
		t.Fatalf("probe update must replace the report, got %s", list[0].ProbedCLIs)
	}

	// Register requires a machine id.
	if err := svc.Register(ctx, Machine{Name: "x"}, "[]"); err == nil {
		t.Fatalf("expected validation error for empty machine_id")
	}
}

// TestUpsertProbeRuntimes covers the Phase 2 runtime provisioning: a probed
// CLI becomes a machine-owned runtime row; a
// re-register refreshes it instead of duplicating.
func TestUpsertProbeRuntimes(t *testing.T) {
	st := newTestStore(t)
	svc := NewMachineService(st)
	ctx := context.Background()

	clis := []link.ProbeCLI{{Name: "claude", Version: "2.1", ACPSpawn: []string{"claude", "--acp"}}}
	if err := svc.UpsertProbeRuntimes(ctx, "m1", "dev", clis); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rtSvc := NewRuntimeService(st)
	all, err := rtSvc.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("expected 1 runtime, got %d (err %v)", len(all), err)
	}
	r := all[0]
	if r.Name != "claude@dev" || r.MachineID != "m1" {
		t.Fatalf("probe runtime mismatch: %+v", r)
	}
	if len(r.Args) != 2 || r.Args[0] != "claude" || r.Args[1] != "--acp" {
		t.Fatalf("acp_spawn must ride args, got %v", r.Args)
	}

	// Re-register (new machine id, same CLI) refreshes the same row.
	if err := svc.UpsertProbeRuntimes(ctx, "m2", "dev", clis); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	all, _ = rtSvc.List(ctx)
	if len(all) != 1 || all[0].MachineID != "m2" {
		t.Fatalf("re-register must refresh, got %+v", all)
	}

	// A second CLI on the same machine adds a second row.
	if err := svc.UpsertProbeRuntimes(ctx, "m2", "dev", []link.ProbeCLI{{Name: "opencode", ACPSpawn: []string{"opencode", "acp", "--pure"}}}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	all, _ = rtSvc.List(ctx)
	if len(all) != 2 {
		t.Fatalf("expected 2 runtimes, got %d", len(all))
	}
}
