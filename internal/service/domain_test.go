package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
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

// TestUpdateDomainTypeGuard covers the edit-path type guard: type is fixed
// at creation, and Update must validate against the DB's real type, not the
// (often-empty) value in the request body. The frontend edit form omits type
// entirely — without the guard, a scratch domain could silently absorb issue
// tracking config (type ↔ capability contradiction) because the scratch
// branch in validateIssueTracking never fires on d.Type == "".
func TestUpdateDomainTypeGuard(t *testing.T) {
	st := newTestStore(t)
	svc := NewDomainService(st, events.NewBus())
	ctx := context.Background()

	// A scratch domain — no repo, so issue tracking must be rejected on edit.
	scratch, err := svc.Create(ctx, Domain{Name: "notebook", Type: "scratch"})
	if err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	if scratch.Type != "scratch" {
		t.Fatalf("expected scratch, got %q", scratch.Type)
	}
	// validateIssueTracking now sees the real type (scratch) and must refuse
	// issue config even though the request body carries no type field.
	if _, err := svc.Update(ctx, scratch.ID, Domain{
		IssueRepo:     "eushing/notebook",
		IssueAssignee: "someone",
	}); err == nil {
		t.Fatalf("expected scratch domain to reject issue tracking on edit")
	}

	// A repo domain — editing without type (the normal frontend path) keeps
	// working and persists the git config.
	repo, err := svc.Create(ctx, Domain{
		Name:   "agentwork",
		Type:   "repo",
		GitURL: "https://github.com/eushing/agentwork.git",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	updated, err := svc.Update(ctx, repo.ID, Domain{
		GitURL:        "https://github.com/eushing/agentwork.git",
		GitCredentials: "token-xyz",
	})
	if err != nil {
		t.Fatalf("update repo without type: %v", err)
	}
	if updated.Type != "repo" || updated.GitCredentials != "token-xyz" {
		t.Fatalf("repo edit round-trip mismatch: %+v", updated)
	}

	// A request that tries to flip the type must be refused loudly — the edit
	// path must never silently swallow a type change into a contradiction.
	if _, err := svc.Update(ctx, repo.ID, Domain{
		Type:   "scratch",
		GitURL: "https://github.com/eushing/agentwork.git",
	}); err == nil {
		t.Fatalf("expected error when editing tries to change type repo→scratch")
	}
	if _, err := svc.Update(ctx, scratch.ID, Domain{
		Type: "repo",
	}); err == nil {
		t.Fatalf("expected error when editing tries to change type scratch→repo")
	}
}

// seedSteward inserts a minimal steward agent (type='steward') bound to a
// throwaway active runtime, returning the agent id. Mirrors SeedSteward.
func seedSteward(t *testing.T, ctx context.Context, st *store.Store) string {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,status,created_at) VALUES ('rt-s','rt-s','active',?)`,
		now()); err != nil {
		t.Fatalf("seed steward runtime: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,type,runtime_id,max_concurrent,created_at) VALUES ('steward-1','steward','steward','rt-s',1,?)`,
		now()); err != nil {
		t.Fatalf("seed steward agent: %v", err)
	}
	return "steward-1"
}

// TestCompilePolicyProcessorAgentFallback covers the CompilePolicy agent
// fallback chain (决策 2-17): request → domain config → steward.
func TestCompilePolicyProcessorAgentFallback(t *testing.T) {
	ctx := context.Background()

	t.Run("domain_configured_agent", func(t *testing.T) {
		st := newTestStore(t)
		bus := events.NewBus()
		domainSvc := NewDomainService(st, bus)
		runSvc := NewRunService(st, bus)
		domainSvc.SetRunService(runSvc)

		procAgentID := seedAgent(t, st, "processor")
		d, err := domainSvc.Create(ctx, Domain{
			Name:            "d1",
			GitURL:          "https://example.com/d1.git",
			ProcessorAgentID: procAgentID,
		})
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		run, err := domainSvc.CompilePolicy(ctx, d.ID, "测试必须通过", "")
		if err != nil {
			t.Fatalf("compile with domain-configured agent: %v", err)
		}
		if run.AgentID != procAgentID {
			t.Fatalf("expected agent %s, got %s", procAgentID, run.AgentID)
		}
	})

	t.Run("steward_default", func(t *testing.T) {
		st := newTestStore(t)
		bus := events.NewBus()
		domainSvc := NewDomainService(st, bus)
		runSvc := NewRunService(st, bus)
		domainSvc.SetRunService(runSvc)

		stewardID := seedSteward(t, ctx, st)
		d, err := domainSvc.Create(ctx, Domain{
			Name:   "d2",
			GitURL: "https://example.com/d2.git",
		})
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		run, err := domainSvc.CompilePolicy(ctx, d.ID, "测试必须通过", "")
		if err != nil {
			t.Fatalf("compile with steward fallback: %v", err)
		}
		if run.AgentID != stewardID {
			t.Fatalf("expected steward %s, got %s", stewardID, run.AgentID)
		}
	})

	t.Run("no_agent_available", func(t *testing.T) {
		st := newTestStore(t)
		bus := events.NewBus()
		domainSvc := NewDomainService(st, bus)
		runSvc := NewRunService(st, bus)
		domainSvc.SetRunService(runSvc)

		d, err := domainSvc.Create(ctx, Domain{
			Name:   "d3",
			GitURL: "https://example.com/d3.git",
		})
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		_, err = domainSvc.CompilePolicy(ctx, d.ID, "测试必须通过", "")
		if err == nil {
			t.Fatalf("expected error when no processor agent available")
		}
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("expected ErrValidation, got %v", err)
		}
		msg := err.Error()
		if strings.Contains(msg, `"" does not exist`) {
			t.Fatalf("error must not leak the empty-id mustExist message, got: %s", msg)
		}
		if !strings.Contains(msg, "no processor agent available") {
			t.Fatalf("expected friendly setup-hint message, got: %s", msg)
		}
	})
}
