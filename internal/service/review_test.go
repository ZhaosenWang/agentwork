package service

import (
	"context"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// seedDomainWithGates creates a domain whose acceptance policy includes the
// merge gate — completed runs park the goal in review (DESIGN.v2.md §4).
func seedDomainWithGates(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	ds := NewDomainService(st, events.NewBus())
	d, err := ds.Create(ctx, Domain{Name: "gated-domain", GitURL: "https://example.com/gated.git"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	d, err = ds.FreezeChecks(ctx, d.ID, Checks{
		Verify: []string{"go test ./..."},
		Gates:  []GateRule{{Name: "merge", When: "合并到主分支前必须人工审批"}},
	}, "strong")
	if err != nil {
		t.Fatalf("freeze checks: %v", err)
	}
	return d.ID
}

// finishWithMergeGate injects the daemon-side gate evaluation result (the
// unit tests don't run the daemon, which computes gates_hit from the diff)
// and finishes the run completed — the goal layer then parks in review.
func finishWithMergeGate(t *testing.T, st *store.Store, rs *RunService, run *Run, summary string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET gates_hit=? WHERE id=?`,
		`["merge: 合并到主分支前必须人工审批"]`, run.ID); err != nil {
		t.Fatalf("set gates_hit: %v", err)
	}
	if err := rs.Finish(ctx, run.ID, "completed", summary); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

// TestReconcileParksInReviewWithGates: a completed run in a gated domain parks
// the goal in review (NOT done) — the checkpoint fires before any promotion.
// Without gates the same run would promote to done.
func TestReconcileParksInReviewWithGates(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, err := gs.Create(ctx, Goal{Title: "gated work", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	finishWithMergeGate(t, st, rs, r, "done")
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "review" {
		t.Fatalf("gated completion must park in review, got %q", after.Status)
	}
	if after.ReviewRequest == "" {
		t.Fatalf("expected review_request with gate reason")
	}
	if after.HandoffNote != "" || after.Status != "review" {
		// note untouched; review owns the state now
	}
}

// TestReviewApproveKeepsGoalParkedUntilDeliver: approve records the
// gate_decision and publishes goal:approved; the goal STAYS in review — the
// daemon's deliver step (merge+re-verify+push) is the only mover, closing
// with MarkDelivered.
func TestReviewApproveKeepsGoalParkedUntilDeliver(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	r := enqueueFirst(t, rs, g)
	_ = r
	finishWithMergeGate(t, st, rs, enqueueFirst(t, rs, g), "ok")

	got, err := gs.ResolveReview(ctx, g.ID, "", "approve", "")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Status != "review" {
		t.Fatalf("approved goal must stay parked until deliver, got %q", got.Status)
	}
	if got.HumanIterations != 0 {
		t.Fatalf("approve must not bump human_iterations")
	}

	// The decision was recorded for the health-learning loop.
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM gate_decision WHERE goal_id=? AND decision='approve'`, g.ID).Scan(&n); err != nil {
		t.Fatalf("count gate_decision: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 gate_decision row, got %d", n)
	}

	// Only MarkDelivered closes the loop.
	done, err := gs.MarkDelivered(ctx, g.ID, true, "merged", nil)
	if err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if done.Status != "done" {
		t.Fatalf("expected done after deliver, got %q", done.Status)
	}
}

// TestReviewRejectSendsBackWithReason: reject records the decision, bumps
// human_iterations (the reject counter — SEPARATE from run.attempt), moves
// the goal back to active with the reason as handoff_note, and enqueues a new
// run with attempt reset — the agent continues from the note.
func TestReviewRejectSendsBackWithReason(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	finishWithMergeGate(t, st, rs, enqueueFirst(t, rs, g), "ok")

	got, err := gs.ResolveReview(ctx, g.ID, "", "reject", "方向不对，把 X 改成 Y 再看")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("rejected goal must return to active, got %q", got.Status)
	}
	if got.HumanIterations != 1 {
		t.Fatalf("expected human_iterations=1, got %d", got.HumanIterations)
	}
	if got.HandoffNote == "" {
		t.Fatalf("reject reason must be carried as handoff_note")
	}
	if got.ReviewRequest != "" {
		t.Fatalf("review_request must be cleared on reject")
	}

	// A fresh run was enqueued on the same assignee, attempt reset to 1.
	runs, _ := rs.List(ctx, g.ID)
	last := runs[len(runs)-1]
	if last.Status != "queued" || last.Attempt != 1 {
		t.Fatalf("expected fresh queued run attempt=1, got %s/%d", last.Status, last.Attempt)
	}
}

// TestReviewRejectThenApproveRoundTrip: after a reject cycle the goal can go
// back to review (completed again) and approve+deliver closes it — the full
// human-iteration loop.
func TestReviewRejectThenApproveRoundTrip(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	finishWithMergeGate(t, st, rs, enqueueFirst(t, rs, g), "ok")
	if _, err := gs.ResolveReview(ctx, g.ID, "", "reject", "改一下"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// Agent fixes; the new run completes → review again.
	runs, _ := rs.List(ctx, g.ID)
	last := runs[len(runs)-1]
	finishWithMergeGate(t, st, rs, &last, "fixed")
	g2, _ := gs.Get(ctx, g.ID)
	if g2.Status != "review" {
		t.Fatalf("expected review again, got %q", g2.Status)
	}
	if _, err := gs.ResolveReview(ctx, g.ID, "", "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done, err := gs.MarkDelivered(ctx, g.ID, true, "merged", nil)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if done.Status != "done" {
		t.Fatalf("expected done, got %q", done.Status)
	}
	if done.HumanIterations != 1 {
		t.Fatalf("expected human_iterations=1, got %d", done.HumanIterations)
	}
}

// TestReviewRecordsRunID: the decision links to the evidence run the human
// judged — the IM approval card carries run_id in its button value, and the
// gate_decision records it (the audit chain, M3).
func TestReviewRecordsRunID(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	run := enqueueFirst(t, rs, g)
	finishWithMergeGate(t, st, rs, run, "ok")

	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var recorded string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT run_id FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, g.ID).Scan(&recorded); err != nil {
		t.Fatalf("read gate_decision: %v", err)
	}
	if recorded != run.ID {
		t.Fatalf("gate_decision.run_id must link the evidence run, got %q want %q", recorded, run.ID)
	}
}

// TestReviewRecordsActualGateRule: gate_decision.gate_rule records WHICH
// rule parked the goal (from the evidence run's gates_hit), not a hardcoded
// "merge" — the health-learning aggregation groups by rule, so a
// diff_contains decision must be recorded as such (regression: every decision
// landed as "merge" and corrupted GateStats).
func TestReviewRecordsActualGateRule(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	run := enqueueFirst(t, rs, g)
	// The daemon records the fired gate on the run row; the goal layer reads
	// it (the daemon computes, the goal layer judges).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET gates_hit=? WHERE id=?`,
		`["diff_contains: 改动含 Go 代码时人工判定测试对应性", "merge: 每次完成需人工审批"]`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, run.ID, "completed", "ok"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var rule string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT gate_rule FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, g.ID).Scan(&rule); err != nil {
		t.Fatalf("read gate_decision: %v", err)
	}
	if rule != "diff_contains" {
		t.Fatalf("gate_rule must name the fired rule, got %q", rule)
	}
}

// TestMarkDeliveredFailureStaysInReview: a failed deliver (merge conflict /
// post-merge verification red) annotates review_request and leaves the goal
// parked — the human can retry deliver or reject the change back.
func TestMarkDeliveredFailureStaysInReview(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	finishWithMergeGate(t, st, rs, enqueueFirst(t, rs, g), "ok")
	if _, err := gs.ResolveReview(ctx, g.ID, "", "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, err := gs.MarkDelivered(ctx, g.ID, false, "合并冲突：internal/acp/client.go", nil)
	if err != nil {
		t.Fatalf("mark delivered failed: %v", err)
	}
	if got.Status != "review" {
		t.Fatalf("failed deliver must stay in review, got %q", got.Status)
	}
	if got.ReviewRequest == "" {
		t.Fatalf("failed deliver must annotate review_request")
	}
}

// TestMentionSuppressedDuringReview: decision 2-3 — a mention comment on a
// goal parked in review lands in the timeline but triggers NO run.
func TestMentionSuppressedDuringReview(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	finishWithMergeGate(t, st, rs, enqueueFirst(t, rs, g), "ok")
	if got, _ := gs.Get(ctx, g.ID); got.Status != "review" {
		t.Fatalf("setup: expected review, got %q", got.Status)
	}

	// Mention B during review: comment lands, no run triggered.
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, Content: "[@B](mention://agent/" + agentB + ") 这里还有问题" }); err != nil {
		t.Fatalf("comment: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "running" {
			t.Fatalf("review must suppress mention-triggered runs, found %s", r.Status)
		}
	}

	// After the review resolves (deliver), a mention triggers normally.
	if _, err := gs.ResolveReview(ctx, g.ID, "", "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := gs.MarkDelivered(ctx, g.ID, true, "merged", nil); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, Content: "[@B](mention://agent/" + agentB + ") 现在可以做了" }); err != nil {
		t.Fatalf("comment 2: %v", err)
	}
	found := false
	runs, _ = rs.List(ctx, g.ID)
	for _, r := range runs {
		if r.AgentID == agentB && (r.Status == "queued" || r.Status == "running") {
			found = true
		}
	}
	if !found {
		t.Fatalf("mention after review must trigger a run on the mentioned agent")
	}
}

// TestReviewRejectBlockedWhileDelivering: approve starts the async deliver and
// the goal stays in review — a reject in that window must be refused (the
// merge may already have pushed; the agent must not restart on a branch whose
// work is already in the default branch). A FAILED deliver re-opens both
// paths (retry deliver / reject back to fix).
func TestReviewRejectBlockedWhileDelivering(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	run := enqueueFirst(t, rs, g)
	finishWithMergeGate(t, st, rs, run, "ok")
	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "approve", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Reject while delivering → refused.
	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "reject", "反悔了"); err == nil {
		t.Fatal("reject during deliver must be refused")
	}
	// Deliver fails (conflict annotated) → reject becomes legal (the agent
	// goes back to fix the conflict).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET review_request=? WHERE id=?`, "deliver: merge conflict in main.go", g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "reject", "解决冲突"); err != nil {
		t.Fatalf("reject after deliver failure must be allowed: %v", err)
	}
}
