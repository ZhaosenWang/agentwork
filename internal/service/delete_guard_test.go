package service

import (
	"context"
	"errors"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// newGuardServices wires the services the delete-guard tests exercise. Each
// shares one bus + store so cross-service references (a schedule pointing at
// an agent, a domain's issue_assignee) resolve against the same DB. Goal+Run
// are cross-wired so CreateSubGoal (which enqueues a run) works.
func newGuardServices(t *testing.T) (*AgentService, *SquadService, *DomainService, *ScheduleService, *GoalService, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	bus := events.NewBus()
	as := NewAgentService(st, bus)
	sq := NewSquadService(st, bus)
	ds := NewDomainService(st, bus)
	ss := NewScheduleService(st, bus)
	gs := NewGoalService(st, bus)
	rs := NewRunService(st, bus)
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	return as, sq, ds, ss, gs, st
}

// wantRefused asserts err is a validation error (the guard fired) and the
// entity row still exists (the delete was blocked, not applied).
func wantRefused(t *testing.T, err error, label string) {
	t.Helper()
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("%s: expected ErrValidation, got %v", label, err)
	}
}

// rowExists reports whether a row with the given id is still present.
func rowExists(t *testing.T, st *store.Store, table, id string) bool {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM `+table+` WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n > 0
}

// TestDeleteAgentRefusedByGuards: each of the six referencing rows blocks the
// delete with a validation error and leaves the agent in place.
func TestDeleteAgentRefusedByGuards(t *testing.T) {
	ctx := context.Background()

	// 1) goal referencing the agent (any status — even terminal blocks).
	{
		as, _, _, _, gs, st := newGuardServices(t)
		agent := seedAgent(t, st, "a-goal")
		dom := seedDomain(t, st)
		if _, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agent, Status: "done", DomainID: dom}); err != nil {
			t.Fatalf("create goal: %v", err)
		}
		err := as.Delete(ctx, agent)
		wantRefused(t, err, "goal guard")
		if !rowExists(t, st, "agent", agent) {
			t.Fatal("agent must survive a refused delete")
		}
	}

	// 2) schedule referencing the agent.
	{
		as, _, ds, ss, _, st := newGuardServices(t)
		agent := seedAgent(t, st, "a-sched")
		dom := seedDomain(t, st)
		if _, err := ds.FreezeChecks(ctx, dom, Checks{}, "medium"); err != nil { // seedDomain already froze; idempotent
			t.Fatalf("freeze: %v", err)
		}
		if _, err := ss.Create(ctx, Schedule{Name: "s", TitleTemplate: "t", AssigneeType: "agent", AssigneeID: agent, DomainID: dom, CronExpression: "*/5 * * * *", Timezone: "UTC"}); err != nil {
			t.Fatalf("create schedule: %v", err)
		}
		err := as.Delete(ctx, agent)
		wantRefused(t, err, "schedule guard")
		if !rowExists(t, st, "agent", agent) {
			t.Fatal("agent must survive a refused delete")
		}
	}

	// 3) squad leadership.
	{
		as, sq, _, _, _, st := newGuardServices(t)
		leader := seedAgent(t, st, "leader")
		if _, err := sq.Create(ctx, Squad{Name: "sq", LeaderID: leader}); err != nil {
			t.Fatalf("create squad: %v", err)
		}
		err := as.Delete(ctx, leader)
		wantRefused(t, err, "squad-leader guard")
		if !rowExists(t, st, "agent", leader) {
			t.Fatal("agent must survive a refused delete")
		}
	}

	// 4) squad membership.
	{
		as, sq, _, _, _, st := newGuardServices(t)
		leader := seedAgent(t, st, "lead-m")
		member := seedAgent(t, st, "member")
		squad, err := sq.Create(ctx, Squad{Name: "sq-m", LeaderID: leader})
		if err != nil {
			t.Fatalf("create squad: %v", err)
		}
		if _, err := sq.AddMember(ctx, squad.ID, "agent", member, ""); err != nil {
			t.Fatalf("add member: %v", err)
		}
		err = as.Delete(ctx, member)
		wantRefused(t, err, "squad-member guard")
		if !rowExists(t, st, "agent", member) {
			t.Fatal("agent must survive a refused delete")
		}
	}

	// 5) running run on the agent.
	{
		as, _, _, _, _, st := newGuardServices(t)
		agent := seedAgent(t, st, "a-run")
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO run (id, goal_id, agent_id, status, queued_at, created_at) VALUES (?, '', ?, 'running', '', '')`,
			"r1", agent); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		err := as.Delete(ctx, agent)
		wantRefused(t, err, "running-run guard")
		if !rowExists(t, st, "agent", agent) {
			t.Fatal("agent must survive a refused delete")
		}
	}

	// 6) issue-handling target (domain.issue_assignee).
	{
		as, _, ds, _, _, st := newGuardServices(t)
		agent := seedAgent(t, st, "a-issue")
		// Build a repo domain whose issue_assignee points at this agent.
		d, err := ds.Create(ctx, Domain{Name: "issue-dom", Type: "repo", GitURL: "https://github.com/o/r.git", IssueRepo: "o/r", IssueAssignee: agent, IssueAssigneeType: "agent", IssueProvider: "github", GitCredentials: "tok"})
		if err != nil {
			t.Fatalf("create issue domain: %v", err)
		}
		_ = d
		err = as.Delete(ctx, agent)
		wantRefused(t, err, "issue-assignee guard")
		if !rowExists(t, st, "agent", agent) {
			t.Fatal("agent must survive a refused delete")
		}
	}
}

// TestDeleteAgentSucceedsAtZeroRefs: with no referencing rows, the delete
// succeeds, drops the agent's run history, and cancels non-terminal sub-goals.
func TestDeleteAgentSucceedsAtZeroRefs(t *testing.T) {
	ctx := context.Background()
	as, _, ds, _, gs, st := newGuardServices(t)
	owner := seedAgent(t, st, "owner")
	worker := seedAgent(t, st, "worker")
	dom := seedDomain(t, st)

	// A goal owned by `owner` with a sub-goal assigned to `worker`. Deleting
	// `worker` must NOT be blocked (the goal points at owner, not worker) and
	// must cancel worker's non-terminal sub-goal.
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: dom})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work", "do it", worker, "", "agent", owner)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// A completed run by worker (history) — must be dropped with the agent.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id, goal_id, agent_id, status, queued_at, created_at) VALUES (?, ?, ?, 'completed', '', '')`,
		"r-done", g.ID, worker); err != nil {
		t.Fatalf("seed completed run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO chat_message (id, run_id, role, content, created_at) VALUES (?, 'r-done', 'assistant', 'hi', '')`,
		"cm1"); err != nil {
		t.Fatalf("seed chat_message: %v", err)
	}

	if err := as.Delete(ctx, worker); err != nil {
		t.Fatalf("delete worker at zero refs: %v", err)
	}
	if rowExists(t, st, "agent", worker) {
		t.Fatal("agent row must be gone after a successful delete")
	}
	if rowExists(t, st, "run", "r-done") {
		t.Fatal("agent's run history must be dropped with the agent")
	}
	if rowExists(t, st, "chat_message", "cm1") {
		t.Fatal("chat_message belonging to a dropped run must be gone")
	}
	after, _ := gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "cancelled" {
		t.Fatalf("non-terminal sub-goal must be cancelled with its assignee, got %q", after.Status)
	}
	// DomainService wiring unused beyond seed — keep ds referenced.
	_ = ds
}

// TestDeleteSquadRefusedByGuards: goal/schedule/issue-assignee each block the
// squad delete and leave the squad in place.
func TestDeleteSquadRefusedByGuards(t *testing.T) {
	ctx := context.Background()

	// 1) goal referencing the squad.
	{
		as, sq, _, _, gs, st := newGuardServices(t)
		leader := seedAgent(t, st, "lead")
		dom := seedDomain(t, st)
		squad, err := sq.Create(ctx, Squad{Name: "sq", LeaderID: leader})
		if err != nil {
			t.Fatalf("create squad: %v", err)
		}
		if _, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "squad", AssigneeID: squad.ID, Status: "active", DomainID: dom}); err != nil {
			t.Fatalf("create goal: %v", err)
		}
		err = sq.Delete(ctx, squad.ID)
		wantRefused(t, err, "squad goal guard")
		if !rowExists(t, st, "squad", squad.ID) {
			t.Fatal("squad must survive a refused delete")
		}
		_ = as
	}

	// 2) schedule referencing the squad.
	{
		_, sq, _, ss, _, st := newGuardServices(t)
		leader := seedAgent(t, st, "lead-s")
		dom := seedDomain(t, st)
		squad, err := sq.Create(ctx, Squad{Name: "sq-s", LeaderID: leader})
		if err != nil {
			t.Fatalf("create squad: %v", err)
		}
		if _, err := ss.Create(ctx, Schedule{Name: "s", TitleTemplate: "t", AssigneeType: "squad", AssigneeID: squad.ID, DomainID: dom, CronExpression: "*/5 * * * *", Timezone: "UTC"}); err != nil {
			t.Fatalf("create schedule: %v", err)
		}
		err = sq.Delete(ctx, squad.ID)
		wantRefused(t, err, "squad schedule guard")
		if !rowExists(t, st, "squad", squad.ID) {
			t.Fatal("squad must survive a refused delete")
		}
	}

	// 3) issue-handling target (domain.issue_assignee = squad).
	{
		_, sq, ds, _, _, st := newGuardServices(t)
		leader := seedAgent(t, st, "lead-i")
		squad, err := sq.Create(ctx, Squad{Name: "sq-i", LeaderID: leader})
		if err != nil {
			t.Fatalf("create squad: %v", err)
		}
		if _, err := ds.Create(ctx, Domain{Name: "issue-dom", Type: "repo", GitURL: "https://github.com/o/r.git", IssueRepo: "o/r", IssueAssignee: squad.ID, IssueAssigneeType: "squad", IssueProvider: "github", GitCredentials: "tok"}); err != nil {
			t.Fatalf("create issue domain: %v", err)
		}
		err = sq.Delete(ctx, squad.ID)
		wantRefused(t, err, "squad issue-assignee guard")
		if !rowExists(t, st, "squad", squad.ID) {
			t.Fatal("squad must survive a refused delete")
		}
	}
}

// TestDeleteSquadSucceedsAtZeroRefs: with no referencing rows, the squad and
// its members are deleted.
func TestDeleteSquadSucceedsAtZeroRefs(t *testing.T) {
	ctx := context.Background()
	_, sq, _, _, _, st := newGuardServices(t)
	leader := seedAgent(t, st, "lead-ok")
	member := seedAgent(t, st, "member-ok")
	squad, err := sq.Create(ctx, Squad{Name: "sq-ok", LeaderID: leader})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := sq.AddMember(ctx, squad.ID, "agent", member, ""); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := sq.Delete(ctx, squad.ID); err != nil {
		t.Fatalf("delete squad at zero refs: %v", err)
	}
	if rowExists(t, st, "squad", squad.ID) {
		t.Fatal("squad row must be gone after a successful delete")
	}
	// squad_member cascades with the squad.
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM squad_member WHERE squad_id=?`, squad.ID).Scan(&n); err != nil {
		t.Fatalf("count squad_member: %v", err)
	}
	if n != 0 {
		t.Fatalf("squad_member rows must cascade with the squad, got %d", n)
	}
	// The leader agent and ordinary member agent are untouched.
	if !rowExists(t, st, "agent", leader) || !rowExists(t, st, "agent", member) {
		t.Fatal("squad delete must not delete the leader or member agents")
	}
}

// TestDeleteDomainRefusedBySchedule: a schedule on the domain blocks the
// domain delete (the goal guard is already covered by existing tests).
func TestDeleteDomainRefusedBySchedule(t *testing.T) {
	ctx := context.Background()
	_, _, ds, ss, _, st := newGuardServices(t)
	agent := seedAgent(t, st, "dom-sched-agent")
	dom := seedDomain(t, st)
	if _, err := ss.Create(ctx, Schedule{Name: "s", TitleTemplate: "t", AssigneeType: "agent", AssigneeID: agent, DomainID: dom, CronExpression: "*/5 * * * *", Timezone: "UTC"}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	err := ds.Delete(ctx, dom)
	wantRefused(t, err, "domain schedule guard")
	if !rowExists(t, st, "domain", dom) {
		t.Fatal("domain must survive a refused delete")
	}
}

// TestDeleteDomainSucceedsAtZeroRefs: with no goal or schedule referencing
// the domain, the delete succeeds.
func TestDeleteDomainSucceedsAtZeroRefs(t *testing.T) {
	ctx := context.Background()
	_, _, ds, _, _, st := newGuardServices(t)
	d, err := ds.Create(ctx, Domain{Name: "free-dom", Type: "repo", GitURL: "https://github.com/o/free.git"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if err := ds.Delete(ctx, d.ID); err != nil {
		t.Fatalf("delete domain at zero refs: %v", err)
	}
	if rowExists(t, st, "domain", d.ID) {
		t.Fatal("domain row must be gone after a successful delete")
	}
}
