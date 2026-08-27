package service

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// TestArchiveAgentStopsRunningRuns: archiving an agent carries its running
// run ids in the agent:archived payload — the daemon cuts the processes from
// them (the rows stay as history, unlike goal:deleted's row-gone reclamation).
// Mirrors TestDeleteCarriesRunningRunIDs's payload-shape assertion (plan §9).
func TestArchiveAgentStopsRunningRuns(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='running' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus()
	as := NewAgentService(st, bus)
	payloadCh := make(chan map[string]any, 1)
	bus.Subscribe("agent:archived", func(_ context.Context, e events.Event) {
		if m, ok := e.Payload.(map[string]any); ok {
			payloadCh <- m
		}
	})
	if err := as.Delete(ctx, agentA); err != nil {
		t.Fatalf("archive agent: %v", err)
	}
	select {
	case m := <-payloadCh:
		ids, ok := m["run_ids"].([]string)
		if !ok || len(ids) != 1 || ids[0] != r.ID {
			t.Fatalf("agent:archived must carry the running run id, got %v", m["run_ids"])
		}
		if m["id"] != agentA {
			t.Fatalf("agent:archived must carry the agent id, got %v", m["id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent:archived event never arrived")
	}
}

// TestArchiveAgentKeepsHistoryAndResolvable: an archived agent is absent from
// List but still returned by Get — audit rows that store the bare id stay
// JOIN-resolvable to a name (the dangling-id cure, plan §4/§7). The run row
// and the agent row both survive.
func TestArchiveAgentKeepsHistoryAndResolvable(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r := enqueueFirst(t, rs, g)

	bus := events.NewBus()
	as := NewAgentService(st, bus)
	if err := as.Delete(ctx, agentA); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	// List excludes the archived agent.
	list, err := as.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.ID == agentA {
			t.Fatal("archived agent must not appear in List")
		}
	}
	// Get still resolves it (audit resolvability).
	got, err := as.Get(ctx, agentA)
	if err != nil {
		t.Fatalf("archived agent must still be Get-able, got %v", err)
	}
	if got.ArchivedAt == "" {
		t.Fatal("archived agent must have ArchivedAt set")
	}
	// The run row survives as history (archive is NOT a cascade delete).
	var runCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE id=?`, r.ID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("archived agent's run row must survive, got count %d", runCount)
	}
}

// TestArchiveAgentDisablesSchedules: an archived agent's schedules are
// disabled (enabled=0), not deleted — they stop firing but are restorable
// (plan §4.7, 对齐 multica pause).
func TestArchiveAgentDisablesSchedules(t *testing.T) {
	_, _, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	ss := NewScheduleService(st, events.NewBus())
	sch, err := ss.Create(ctx, Schedule{
		Name: "nightly", TitleTemplate: "nightly build",
		AssigneeType: "agent", AssigneeID: agentA, DomainID: domID,
		CronExpression: "0 2 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	bus := events.NewBus()
	as := NewAgentService(st, bus)
	if err := as.Delete(ctx, agentA); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	var enabled int
	if err := st.DB().QueryRowContext(ctx, `SELECT enabled FROM schedule WHERE id=?`, sch.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("archived agent's schedule must be disabled (enabled=0), got %d", enabled)
	}
	// The schedule row survives (restorable).
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schedule WHERE id=?`, sch.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("archived agent's schedule row must survive")
	}
}

// TestArchiveSquadTransfersGoalToLeader: archiving a squad transfers its
// goals to the leader agent (NOT dropped to human — the leader inherits,
// 对齐 multica TransferSquadAssignees, plan §5). The goal stays active under
// a new agent owner.
func TestArchiveSquadTransfersGoalToLeader(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")
	domID := seedDomain(t, st)

	bus := events.NewBus()
	squadSvc := NewSquadService(st, bus)
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leader})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "sg", AssigneeType: "squad", AssigneeID: sq.ID, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create squad goal: %v", err)
	}

	if err := squadSvc.Delete(ctx, sq.ID); err != nil {
		t.Fatalf("archive squad: %v", err)
	}
	got, err := gs.Get(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AssigneeType != "agent" || got.AssigneeID != leader {
		t.Fatalf("squad goal must transfer to the leader agent, got type=%q id=%q", got.AssigneeType, got.AssigneeID)
	}
}

// TestArchiveSquadClearsIssueAssignee: archiving a squad clears any
// domain.issue_assignee that pointed at it — an archived squad must not keep
// auto-creating issues from its repo (plan §5, high-priority gap ③).
func TestArchiveSquadClearsIssueAssignee(t *testing.T) {
	_, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")

	// Seed a domain with issue_assignee pointing at the squad. seedDomain
	// freezes an empty policy; we patch the issue fields directly afterward.
	domID := seedDomain(t, st)
	bus := events.NewBus()
	squadSvc := NewSquadService(st, bus)
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leader})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE domain SET issue_repo='owner/repo', issue_assignee=?, issue_assignee_type='squad' WHERE id=?`, sq.ID, domID); err != nil {
		t.Fatal(err)
	}

	if err := squadSvc.Delete(ctx, sq.ID); err != nil {
		t.Fatalf("archive squad: %v", err)
	}
	var assignee, atype string
	if err := st.DB().QueryRowContext(ctx, `SELECT issue_assignee, issue_assignee_type FROM domain WHERE id=?`, domID).Scan(&assignee, &atype); err != nil {
		t.Fatal(err)
	}
	if assignee != "" || atype != "agent" {
		t.Fatalf("archived squad's issue_assignee must be cleared, got assignee=%q type=%q", assignee, atype)
	}
}

// TestArchiveSquadStopsLeaderRun: archiving a squad carries its leader's
// running run ids in the squad:archived payload — the daemon cuts them (plan
// §5). A leader run has run.squad_id set + is_leader_run=1.
func TestArchiveSquadStopsLeaderRun(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")
	domID := seedDomain(t, st)

	bus := events.NewBus()
	squadSvc := NewSquadService(st, bus)
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leader})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "sg", AssigneeType: "squad", AssigneeID: sq.ID, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create squad goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	// Stamp it as a running leader run belonging to this squad.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', is_leader_run=1, squad_id=? WHERE id=?`, sq.ID, r.ID); err != nil {
		t.Fatal(err)
	}

	payloadCh := make(chan map[string]any, 1)
	bus.Subscribe("squad:archived", func(_ context.Context, e events.Event) {
		if m, ok := e.Payload.(map[string]any); ok {
			payloadCh <- m
		}
	})
	if err := squadSvc.Delete(ctx, sq.ID); err != nil {
		t.Fatalf("archive squad: %v", err)
	}
	select {
	case m := <-payloadCh:
		ids, ok := m["run_ids"].([]string)
		if !ok || len(ids) != 1 || ids[0] != r.ID {
			t.Fatalf("squad:archived must carry the leader run id, got %v", m["run_ids"])
		}
		if m["leader_id"] != leader {
			t.Fatalf("squad:archived must carry the leader id, got %v", m["leader_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("squad:archived event never arrived")
	}
}

// TestArchiveAgentLeaderRestrict: an agent that leads a squad cannot be
// archived — the caller must reassign the squad's leader first (squad.leader_id
// is RESTRICT; a leaderless squad is invalid). The guard is preserved from
// the old hard-delete path (plan §4.2).
func TestArchiveAgentLeaderRestrict(t *testing.T) {
	_, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")

	squadSvc := NewSquadService(st, events.NewBus())
	if _, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leader}); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	bus := events.NewBus()
	as := NewAgentService(st, bus)
	err := as.Delete(ctx, leader)
	if err == nil {
		t.Fatal("archiving a squad leader must be rejected")
	}
}
