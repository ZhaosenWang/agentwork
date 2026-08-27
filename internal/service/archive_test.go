package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// TestArchiveAgentRefusesRunningRun: archiving an agent with a running run is
// refused — the operator stops the run first (plan §方案二, 反馈3). Once stopped,
// the archive succeeds and carries an empty run_ids (the guard is the source of
// truth; the daemon's cancelRun path is a no-op backstop).
func TestArchiveAgentRefusesRunningRun(t *testing.T) {
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
	fired := make(chan struct{}, 1)
	bus.Subscribe("agent:archived", func(_ context.Context, _ events.Event) { fired <- struct{}{} })

	// A running run blocks the archive — the operator must stop it first.
	if err := as.Delete(ctx, agentA); !errors.Is(err, ErrValidation) {
		t.Fatalf("archive with a running run must be refused, got %v", err)
	}
	// No event fired — the guard rejected before publishing.
	select {
	case <-fired:
		t.Fatal("agent:archived must not fire when the guard refused")
	default:
	}
	// The agent is still active (not archived).
	got, err := as.Get(ctx, agentA)
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchivedAt != "" {
		t.Fatalf("agent must remain active, archived_at=%q", got.ArchivedAt)
	}

	// Once the run is no longer running, the archive succeeds and carries an
	// empty run_ids (the guard, not the daemon, is the source of truth).
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='completed' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	payloadCh := make(chan map[string]any, 1)
	bus.Subscribe("agent:archived", func(_ context.Context, e events.Event) {
		if m, ok := e.Payload.(map[string]any); ok {
			payloadCh <- m
		}
	})
	if err := as.Delete(ctx, agentA); err != nil {
		t.Fatalf("archive after run stopped: %v", err)
	}
	select {
	case m := <-payloadCh:
		ids, _ := m["run_ids"].([]string)
		if len(ids) != 0 {
			t.Fatalf("agent:archived run_ids must be empty post-guard, got %v", ids)
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
	list, err := as.List(ctx, false)
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
func TestArchiveSquadRefusesRunningRun(t *testing.T) {
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

	fired := make(chan struct{}, 1)
	bus.Subscribe("squad:archived", func(_ context.Context, _ events.Event) { fired <- struct{}{} })

	// A running run blocks the archive — the operator must stop it first.
	if err := squadSvc.Delete(ctx, sq.ID); !errors.Is(err, ErrValidation) {
		t.Fatalf("archive squad with a running run must be refused, got %v", err)
	}
	select {
	case <-fired:
		t.Fatal("squad:archived must not fire when the guard refused")
	default:
	}

	// Once stopped, the archive succeeds and carries an empty run_ids.
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='completed' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	payloadCh := make(chan map[string]any, 1)
	bus.Subscribe("squad:archived", func(_ context.Context, e events.Event) {
		if m, ok := e.Payload.(map[string]any); ok {
			payloadCh <- m
		}
	})
	if err := squadSvc.Delete(ctx, sq.ID); err != nil {
		t.Fatalf("archive squad after run stopped: %v", err)
	}
	select {
	case m := <-payloadCh:
		ids, _ := m["run_ids"].([]string)
		if len(ids) != 0 {
			t.Fatalf("squad:archived run_ids must be empty post-guard, got %v", ids)
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

// TestAssignRefusesArchivedAgent: after an agent is archived, assigning a goal
// or creating a schedule to it is refused — active work must not land on a
// soft-deleted agent (方案三, 反馈4).
func TestAssignRefusesArchivedAgent(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	as := NewAgentService(st, events.NewBus())
	if err := as.Delete(ctx, agentA); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Goal assignment is refused.
	if _, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID}); !errors.Is(err, ErrValidation) {
		t.Fatalf("create goal on archived agent must be refused, got %v", err)
	}
	// Schedule creation is refused.
	ss := NewScheduleService(st, events.NewBus())
	if _, err := ss.Create(ctx, Schedule{Name: "s", TitleTemplate: "t", AssigneeType: "agent", AssigneeID: agentA, DomainID: domID, CronExpression: "*/5 * * * *", Timezone: "UTC"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("create schedule on archived agent must be refused, got %v", err)
	}
}

// TestAssignRefusesSquadWithArchivedMember: a squad whose roster contains an
// archived agent cannot be assigned (goal or schedule) — the roster is
// incomplete and the leader would dispatch to a dead agent id. The squad row
// itself stays active; the archived member is the blocker (方案三, 反馈4).
func TestAssignRefusesSquadWithArchivedMember(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")
	member := seedAgent(t, st, "member")
	domID := seedDomain(t, st)

	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leader})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", member, "reviewer"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Archive the member (not the leader — leader is RESTRICT-protected).
	as := NewAgentService(st, events.NewBus())
	if err := as.Delete(ctx, member); err != nil {
		t.Fatalf("archive member: %v", err)
	}

	// Assigning the squad to a goal is refused (roster incomplete).
	if _, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "squad", AssigneeID: sq.ID, Status: "active", DomainID: domID}); !errors.Is(err, ErrValidation) {
		t.Fatalf("create goal on squad with archived member must be refused, got %v", err)
	}
	// Creating a schedule on this squad is refused.
	ss := NewScheduleService(st, events.NewBus())
	if _, err := ss.Create(ctx, Schedule{Name: "s", TitleTemplate: "t", AssigneeType: "squad", AssigneeID: sq.ID, DomainID: domID, CronExpression: "*/5 * * * *", Timezone: "UTC"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("create schedule on squad with archived member must be refused, got %v", err)
	}
}

// TestArchiveAgentPreservesSquadMembership: archiving an agent keeps its
// squad_member rows — the roster still lists it (with a "已删除" tag in the UI),
// and restore is trivial. Completeness is judged by joining agent.archived_at,
// not by the row's presence (方案一, 反馈2).
func TestArchiveAgentPreservesSquadMembership(t *testing.T) {
	_, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")
	member := seedAgent(t, st, "member")

	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leader})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", member, "reviewer"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	as := NewAgentService(st, events.NewBus())
	if err := as.Delete(ctx, member); err != nil {
		t.Fatalf("archive member: %v", err)
	}

	// The squad_member row survives — roster still lists the archived agent.
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad_member WHERE squad_id=? AND member_type='agent' AND member_id=?`, sq.ID, member).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("squad_member row must survive archive, got count=%d", n)
	}
}

// TestScheduleEnableRevalidatesAssignee: enabling a schedule whose agent was
// archived after creation is refused — the operator must reassign before
// resuming firing (方案三, 反馈5).
func TestScheduleEnableRevalidatesAssignee(t *testing.T) {
	_, _, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	ss := NewScheduleService(st, events.NewBus())
	sch, err := ss.Create(ctx, Schedule{Name: "s", TitleTemplate: "t", AssigneeType: "agent", AssigneeID: agentA, DomainID: domID, CronExpression: "*/5 * * * *", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	// Disable it, then archive the agent.
	if _, err := ss.SetEnabled(ctx, sch.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	as := NewAgentService(st, events.NewBus())
	if err := as.Delete(ctx, agentA); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// Re-enabling must be refused (assignee archived).
	if _, err := ss.SetEnabled(ctx, sch.ID, true); !errors.Is(err, ErrValidation) {
		t.Fatalf("enable schedule on archived agent must be refused, got %v", err)
	}
}
