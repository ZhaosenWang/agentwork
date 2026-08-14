package service

import (
	"context"
	"strings"
	"testing"
)

// ── Wake note (决策 6-17): the owner-wake reason travels on the RUN row ──
//
// The wrapup predicate self-invalidates once the owner spawn lands (its
// queued_at resets the recency window), so the events that follow the spawn
// (sub_goal.verified, run.terminal) re-derive goal.attention to ''. A woken
// owner reading the live attention column would lose its wake context — the
// live failure these tests pin: the owner run's wake_note must survive
// subsequent reconciles, and must name the concrete work items in the goal's
// language (决策 6-18).

// TestWakeNoteSurvivesReconcile: the no-code wrapup shape (verified sub-goal,
// no Change) spawns an owner run whose wake_note names the sub-goal — and a
// SECOND reconcile (the exact event-order race from the live log) must not
// erase it. Platform text is English (决策 6-18); the sub-goal title (the
// MATERIAL) carries its own language.
func TestWakeNoteSurvivesReconcile(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	g, err := gs.Create(ctx, Goal{Title: "看看README有没有问题", Description: "看看README有没有问题", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	// Age out the create-time owner run (the progress guard compares signal
	// recency against the last owner spawn — an old one lets the wrapup
	// signal through; a COMPLETED one skips the coalesce and the
	// cancelled-spawn guard).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', queued_at='2020-01-01T00:00:00Z', finished_at='2020-01-01T00:01:00Z' WHERE goal_id=? AND role='owner'`, g.ID); err != nil {
		t.Fatal(err)
	}
	// The verified sub-goal: its run finished just now, no Change — the
	// deliverable lives in the feed (决策 6-8).
	sg, err := gs.CreateSubGoal(ctx, g.ID, "审核并修复 README.md", "核对 README 声明", owner, "", "agent", owner)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='verified' WHERE id=?`, sg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE sub_goal_id=? AND role='subgoal'`, now(), sg.ID); err != nil {
		t.Fatal(err)
	}

	// First reconcile: wrapup attention → owner spawn with the note.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	var note1 string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT wake_note FROM run WHERE goal_id=? AND role='owner' AND status='queued' ORDER BY queued_at DESC LIMIT 1`, g.ID).Scan(&note1); err != nil {
		t.Fatalf("spawned owner run: %v", err)
	}
	if !strings.Contains(note1, "审核并修复 README.md") {
		t.Fatalf("the wake note must name the verified sub-goal, got %q", note1)
	}
	if !strings.Contains(note1, "verified with no changes") {
		t.Fatalf("the wake note must carry the wrapup semantics, got %q", note1)
	}

	// Second reconcile — the live race: the owner's queued_at now postdates
	// the sub-goal run, so the wrapup predicate is false and attention
	// re-derives to ''. The note on the run row must NOT change.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	var note2 string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT wake_note FROM run WHERE goal_id=? AND role='owner' AND status='queued' ORDER BY queued_at DESC LIMIT 1`, g.ID).Scan(&note2); err != nil {
		t.Fatal(err)
	}
	if note2 != note1 {
		t.Fatalf("a later reconcile must not erase the spawn-time wake note:\nbefore %q\nafter  %q", note1, note2)
	}
}

// TestWakeNoteRecoveryAndReadyEnglish: recovery + ready-change bits render
// in English for an English goal, naming the failed sub-goal and the change
// count.
func TestWakeNoteRecoveryAndReadyEnglish(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	g, err := gs.Create(ctx, Goal{Title: "Fix the auth flow", Description: "Make login work", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', queued_at='2020-01-01T00:00:00Z', finished_at='2020-01-01T00:01:00Z' WHERE goal_id=? AND role='owner'`, g.ID); err != nil {
		t.Fatal(err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "Implement OAuth", "Add the OAuth handshake", owner, "", "agent", owner)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// Failed sub-goal + a ready change: recovery + integration both armed.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='failed' WHERE id=?`, sg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='failed', finished_at=? WHERE sub_goal_id=? AND role='subgoal'`, now(), sg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO change (id,goal_id,sub_goal_id,status,created_at) VALUES (?,?,?,'ready',?)`,
		newID(), g.ID, sg.ID, now()); err != nil {
		t.Fatal(err)
	}

	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	var note string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT wake_note FROM run WHERE goal_id=? AND role='owner' AND status='queued' ORDER BY queued_at DESC LIMIT 1`, g.ID).Scan(&note); err != nil {
		t.Fatalf("spawned owner run: %v", err)
	}
	if !strings.Contains(note, "Implement OAuth") || !strings.Contains(note, "failed") {
		t.Fatalf("the wake note must name the failed sub-goal, got %q", note)
	}
	if !strings.Contains(note, "1 change(s) ready to integrate") {
		t.Fatalf("the wake note must name the ready change count, got %q", note)
	}
	if strings.Contains(note, "verified with no changes") {
		t.Fatalf("a sub-goal WITH a change must not be named in the no-changes bullet, got %q", note)
	}
}
