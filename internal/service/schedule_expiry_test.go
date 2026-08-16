package service

import (
	"context"
	"testing"
	"time"
)

// TestExpireStaleScheduledRuns: a schedule-fired run that sat queued past
// the TTL is a miss ("cron: miss = miss") — the fired goal is cancelled
// (no retry chain) and the schedule_run row is stamped missed. A FRESH
// queued run must survive the sweep.
func TestExpireStaleScheduledRuns(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agent := seedAgent(t, st, "A")
	domainID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "sched", AssigneeType: "agent", AssigneeID: agent, Status: "active", DomainID: domainID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.EnqueueForGoal(ctx, *g); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO schedule (id,name,title_template,assignee_id,cron_expression,created_at) VALUES (?,?,?,?,?,?)`,
		"sch-1", "s", "sched", agent, "0 9 * * *", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO schedule_run (id,schedule_id,goal_id,planned_at,status,created_at) VALUES (?,?,?,?,?,?)`,
		"sr-1", "sch-1", g.ID, now(), "dispatched", now()); err != nil {
		t.Fatal(err)
	}
	// Backdate the queued run past the TTL.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET queued_at=? WHERE goal_id=?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), g.ID); err != nil {
		t.Fatal(err)
	}
	n, err := rs.ExpireStaleScheduledRuns(ctx, time.Now().Add(-30*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("expected 1 expired run, got %d (err %v)", n, err)
	}
	var goalStatus, runStatus, srStatus string
	_ = st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, g.ID).Scan(&goalStatus)
	_ = st.DB().QueryRowContext(ctx, `SELECT status FROM run WHERE goal_id=?`, g.ID).Scan(&runStatus)
	_ = st.DB().QueryRowContext(ctx, `SELECT status FROM schedule_run WHERE goal_id=?`, g.ID).Scan(&srStatus)
	if goalStatus != "cancelled" || runStatus != "cancelled" || srStatus != "missed" {
		t.Fatalf("goal=%s run=%s schedule_run=%s (want cancelled/cancelled/missed)", goalStatus, runStatus, srStatus)
	}

	// A fresh schedule-fired run (queued just now) survives the sweep.
	g2, err := gs.Create(ctx, Goal{Title: "sched2", AssigneeType: "agent", AssigneeID: agent, Status: "active", DomainID: domainID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.EnqueueForGoal(ctx, *g2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO schedule (id,name,title_template,assignee_id,cron_expression,created_at) VALUES (?,?,?,?,?,?)`,
		"sch-2", "s2", "sched2", agent, "0 9 * * *", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO schedule_run (id,schedule_id,goal_id,planned_at,status,created_at) VALUES (?,?,?,?,?,?)`,
		"sr-2", "sch-2", g2.ID, now(), "dispatched", now()); err != nil {
		t.Fatal(err)
	}
	n, err = rs.ExpireStaleScheduledRuns(ctx, time.Now().Add(-30*time.Minute))
	if err != nil || n != 0 {
		t.Fatalf("a fresh queued run must survive the sweep, got %d (err %v)", n, err)
	}
}
