package service

import (
	"context"
	"testing"

	"github.com/eushing/agentwork/internal/events"
)

// TestCompilePolicyEnqueuesProcessorRun covers the compile kickoff: the NL
// intent is recorded on the domain and a processor run is enqueued for the
// given processor agent (coalescing a second kickoff of the same domain).
func TestCompilePolicyEnqueuesProcessorRun(t *testing.T) {
	st := newTestStore(t)
	bus := events.NewBus()
	domainSvc := NewDomainService(st, bus)
	runSvc := NewRunService(st, bus)
	domainSvc.SetRunService(runSvc)
	ctx := context.Background()

	d, err := domainSvc.Create(ctx, Domain{Name: "aw", GitURL: "https://example.com/aw.git"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}

	procAgentID := seedAgent(t, st, "processor")
	run, err := domainSvc.CompilePolicy(ctx, d.ID, "测试必须通过，改动要带测试", procAgentID)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if run.RunKind != "processor" || run.DomainID != d.ID || run.GoalID != "" {
		t.Fatalf("processor run mismatch: %+v", run)
	}
	if run.Prompt == "" {
		t.Fatalf("expected compile prompt")
	}

	// Policy text was recorded on the domain.
	got, err := domainSvc.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got.PolicyText == "" {
		t.Fatalf("expected policy_text recorded")
	}

	// Second kickoff coalesces (same domain + agent still queued).
	run2, err := domainSvc.CompilePolicy(ctx, d.ID, "新的要求", procAgentID)
	if err != nil {
		t.Fatalf("compile again: %v", err)
	}
	if run2.ID != run.ID {
		t.Fatalf("expected coalesced processor run, got %s vs %s", run2.ID, run.ID)
	}

	// Validation: empty policy rejected; unknown processor agent rejected.
	if _, err := domainSvc.CompilePolicy(ctx, d.ID, "  ", procAgentID); err == nil {
		t.Fatalf("expected validation error for empty policy")
	}
	if _, err := domainSvc.CompilePolicy(ctx, d.ID, "x", "nope"); err == nil {
		t.Fatalf("expected validation error for unknown processor agent")
	}
}

// TestLatestCompileRun covers the compile panel's refresh-restore query
// (决策 6-23): no run → null; active run → returned as-is; after the run
// goes terminal a newer compile wins — the panel branches on status.
func TestLatestCompileRun(t *testing.T) {
	st := newTestStore(t)
	bus := events.NewBus()
	domainSvc := NewDomainService(st, bus)
	runSvc := NewRunService(st, bus)
	domainSvc.SetRunService(runSvc)
	runSvc.SetGoalService(NewGoalService(st, bus)) // Finish reconciles; goal-less processor runs no-op there
	ctx := context.Background()

	// Never compiled → null, no error.
	got, err := runSvc.LatestCompileRun(ctx, "nope")
	if err != nil || got != nil {
		t.Fatalf("expected (nil, nil) for unknown domain, got (%+v, %v)", got, err)
	}

	d, err := domainSvc.Create(ctx, Domain{Name: "aw", GitURL: "https://example.com/aw.git"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	procAgentID := seedAgent(t, st, "processor")
	run, err := domainSvc.CompilePolicy(ctx, d.ID, "测试必须通过", procAgentID)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Active run is returned with its real row (status queued).
	latest, err := runSvc.LatestCompileRun(ctx, d.ID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest == nil || latest.ID != run.ID || latest.Status != "queued" || latest.AgentID != procAgentID {
		t.Fatalf("expected the queued compile run, got %+v", latest)
	}

	// Terminal run: the failure (and its summary) survives for the panel.
	if err := runSvc.Finish(ctx, run.ID, "failed", "依赖安装失败"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	latest, err = runSvc.LatestCompileRun(ctx, d.ID)
	if err != nil {
		t.Fatalf("latest after finish: %v", err)
	}
	if latest == nil || latest.Status != "failed" || latest.ResultSummary != "依赖安装失败" {
		t.Fatalf("expected the failed run with summary, got %+v", latest)
	}

	// A newer compile wins over the terminal one.
	run2, err := domainSvc.CompilePolicy(ctx, d.ID, "新的要求", procAgentID)
	if err != nil {
		t.Fatalf("compile again: %v", err)
	}
	latest, err = runSvc.LatestCompileRun(ctx, d.ID)
	if err != nil {
		t.Fatalf("latest after recompile: %v", err)
	}
	if latest == nil || latest.ID != run2.ID {
		t.Fatalf("expected the newest run %s, got %+v", run2.ID, latest)
	}
}

// TestDomainCRUDAndFreeze covers the domain lifecycle: create with defaults,
// read back, and freeze the compiled acceptance policy (the owner-confirmed
// step that keeps the "define" role with the human — DESIGN.md §5.3).
func TestDomainCRUDAndFreeze(t *testing.T) {
	st := newTestStore(t)
	svc := NewDomainService(st, events.NewBus())
	ctx := context.Background()

	d, err := svc.Create(ctx, Domain{
		Name:       "agentwork",
		GitURL:     "https://github.com/eushing/agentwork.git",
		PolicyText: "测试必须通过，改动要带测试",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Type != "repo" {
		t.Fatalf("expected default type repo, got %q", d.Type)
	}
	if d.ChecksCompiledAt != "" {
		t.Fatalf("expected uncompiled on create")
	}
	if d.MaxRunDuration != 7200 {
		t.Fatalf("expected default max_run_duration 7200, got %d", d.MaxRunDuration)
	}
	if d.VerifyTimeout != 600 {
		t.Fatalf("expected default verify_timeout 600, got %d", d.VerifyTimeout)
	}

	got, err := svc.Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "agentwork" || got.PolicyText == "" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Freeze the compiled policy after owner confirmation.
	frozen, err := svc.FreezeChecks(ctx, d.ID, Checks{
		Verify: []string{"go test ./..."},
		Guards: []Guard{{Type: "diff_contains", Pattern: "*_test.go"}},
		Gates:  []GateRule{{Name: "merge", When: "合并到主分支前必须人工审批"}},
	}, "strong")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if frozen.ChecksCompiledAt == "" {
		t.Fatalf("expected checks frozen after confirm")
	}
	if len(frozen.Checks.Verify) != 1 || frozen.Checks.Verify[0] != "go test ./..." {
		t.Fatalf("checks mismatch: %+v", frozen.Checks)
	}
	if frozen.VerificationStrength != "strong" {
		t.Fatalf("strength mismatch: %q", frozen.VerificationStrength)
	}

	// Frozen checks round-trip through List too.
	all, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || len(all[0].Checks.Gates) != 1 {
		t.Fatalf("list mismatch: %+v", all)
	}

	// Validation gates.
	if _, err := svc.Create(ctx, Domain{Name: "x"}); err == nil {
		t.Fatalf("expected validation error for missing git_url")
	}
	if _, err := svc.FreezeChecks(ctx, d.ID, Checks{}, "unknown"); err == nil {
		t.Fatalf("expected validation error for bad strength")
	}
	if _, err := svc.Get(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
