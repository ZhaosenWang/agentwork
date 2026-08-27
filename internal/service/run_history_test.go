package service

import (
	"context"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// TestHistoryByAgentJoinsGoalAndOrders: the chat "what have I done" view joins
// each run to its goal (title + goal status ride alongside the run outcome),
// excludes processor runs, filters by run.status, and returns newest-finished
// first. The agent in chat has no run token — this query is its only window
// onto past work, so the join + ordering must be right.
func TestHistoryByAgentJoinsGoalAndOrders(t *testing.T) {
	_, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	dom := seedDomainWithGates(t, st)

	// Two goals for agent A, one for agent B.
	g1 := mustCreateGoal(t, st, "重构登录", agentA, dom, "active")
	g2 := mustCreateGoal(t, st, "修复内存泄漏", agentA, dom, "done")
	g3 := mustCreateGoal(t, st, "B的活", agentB, dom, "active")

	// Agent A: a completed run on g1 (newer), a failed run on g2 (older),
	// and a processor run that must be EXCLUDED.
	mustInsertRun(t, st, "run-g1", g1, agentA, "worker", "completed", "2026-08-26T10:00:00Z", "重构完成，测试通过")
	mustInsertRun(t, st, "run-g2", g2, agentA, "worker", "failed", "2026-08-25T10:00:00Z", "未能复现泄漏")
	mustInsertProcessorRun(t, st, "run-proc", g1, agentA, "2026-08-27T10:00:00Z") // newest but excluded
	// Agent B's run must not appear for agent A.
	mustInsertRun(t, st, "run-g3", g3, agentB, "worker", "completed", "2026-08-27T10:00:00Z", "done")

	items, err := rs.HistoryByAgent(ctx, agentA, 0, "")
	if err != nil {
		t.Fatalf("HistoryByAgent: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 worker runs for agent A (processor excluded, B's run excluded), got %d: %+v", len(items), items)
	}
	// Newest finished first.
	if items[0].RunID != "run-g1" || items[0].GoalTitle != "重构登录" {
		t.Fatalf("first item must be the newer completed run on g1, got %+v", items[0])
	}
	if items[1].RunID != "run-g2" || items[1].RunStatus != "failed" {
		t.Fatalf("second item must be the older failed run on g2, got %+v", items[1])
	}
	// The goal status rode along (g2 is done).
	if items[1].GoalStatus != "done" {
		t.Fatalf("goal status must ride the join, got %+v", items[1])
	}

	// Status filter: only failed.
	failed, err := rs.HistoryByAgent(ctx, agentA, 0, "failed")
	if err != nil {
		t.Fatalf("HistoryByAgent failed: %v", err)
	}
	if len(failed) != 1 || failed[0].RunID != "run-g2" {
		t.Fatalf("status=failed must return only run-g2, got %+v", failed)
	}

	// Limit truncates.
	one, err := rs.HistoryByAgent(ctx, agentA, 1, "")
	if err != nil {
		t.Fatalf("HistoryByAgent limit=1: %v", err)
	}
	if len(one) != 1 || one[0].RunID != "run-g1" {
		t.Fatalf("limit=1 must return only the newest, got %+v", one)
	}
}

func mustCreateGoal(t *testing.T, st *store.Store, title, agentID, domID, status string) string {
	t.Helper()
	g, err := NewGoalService(st, events.NewBus()).Create(context.Background(), Goal{
		Title: title, AssigneeType: "agent", AssigneeID: agentID, Status: status, DomainID: domID,
	})
	if err != nil {
		t.Fatalf("create goal %s: %v", title, err)
	}
	return g.ID
}

func mustInsertRun(t *testing.T, st *store.Store, id, goalID, agentID, kind, status, finishedAt, summary string) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, goalID, agentID, kind, "", status, "owner", 1, summary, finishedAt, finishedAt, finishedAt); err != nil {
		t.Fatalf("insert run %s: %v", id, err)
	}
}

func mustInsertProcessorRun(t *testing.T, st *store.Store, id, goalID, agentID, at string) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,finished_at,queued_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, goalID, agentID, "processor", "compile", "completed", "", 1, at, at, at); err != nil {
		t.Fatalf("insert processor run %s: %v", id, err)
	}
}
