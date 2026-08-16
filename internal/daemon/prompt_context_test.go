package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// ── The engineered context system (决策 6-22) ──
//
// Every prompt = the FIXED BLOCK (once per session) + the WAKE LINE (every
// turn). The feed is PULLED, never injected; AGENTWORK.md is retired.

// seedCtx builds the store bits the context builders read.
func seedCtx(t *testing.T) (*Daemon, *store.Store, string, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{st: st}
	rt, err := service.NewRuntimeService(st).Create(context.Background(), service.Runtime{Name: "rt", MachineID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	agentB, err := service.NewAgentService(st, events.NewBus()).Create(context.Background(), service.Agent{Name: "B", RuntimeID: rt.ID})
	if err != nil {
		t.Fatal(err)
	}
	dom, err := service.NewDomainService(st, events.NewBus()).Create(context.Background(), service.Domain{Name: "d", GitURL: "https://example.com/d.git", PolicyText: "测试能过"})
	if err != nil {
		t.Fatal(err)
	}
	gs := service.NewGoalService(st, events.NewBus())
	gs.SetRunService(service.NewRunService(st, events.NewBus()))
	g, err := gs.Create(context.Background(), service.Goal{Title: "g", Description: "desc", AssigneeType: "agent", AssigneeID: agentB.ID, Status: "active", DomainID: dom.ID})
	if err != nil {
		t.Fatal(err)
	}
	return d, st, g.ID, agentB.ID
}

// TestFixedBlockShape: every section is present, materials keep their
// language — the TEAM is NOT part of the prompt anymore (it rides the
// workdir's AGENTS.md, see TestTeamProfile).
func TestFixedBlockShape(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	block := d.buildFixedBlock(ctx, goalID, agentID, "B", "owner", "g", "测试能过", "repo", "")

	for _, want := range []string{
		"# Background & Requirements", "# Goal", "# Who You Are", "# Tools",
		"- Title: g", "- Acceptance policy: 测试能过",
		"agentwork goal comments", "agentwork help",
		"WITHOUT --after", // the no-memory contract
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("fixed block must carry %q, got:\n%s", want, block)
		}
	}
	if strings.Contains(block, "# Team") || strings.Contains(block, "agentwork MCP") {
		t.Fatalf("the prompt must not carry the team block or MCP-era names, got:\n%s", block)
	}
	_ = st
}

// TestFixedBlockRoleContracts: each role gets its own behavioral contract —
// the reviewer is told review-only, the owner the dispatch rules.
func TestFixedBlockRoleContracts(t *testing.T) {
	d, _, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	owner := d.buildFixedBlock(ctx, goalID, agentID, "B", "owner", "g", "", "repo", "")
	for _, want := range []string{"final message becomes your run's report", "never write ids", "JUDGED, not declared", "agentwork subgoal create --title T --assignee <agent-id>"} {
		if !strings.Contains(owner, want) {
			t.Fatalf("the owner contract must carry %q", want)
		}
	}
	reviewer := d.buildFixedBlock(ctx, goalID, agentID, "B", "review", "g", "", "repo", "")
	if !strings.Contains(reviewer, "REVIEW ONLY") || !strings.Contains(reviewer, "never do the work") {
		t.Fatalf("the reviewer contract must be review-only, got:\n%s", reviewer)
	}
	sub := d.buildFixedBlock(ctx, goalID, agentID, "B", "subgoal", "g", "", "repo", "")
	if !strings.Contains(sub, "NEVER post your conclusions with `agentwork goal comment`") {
		t.Fatalf("the subgoal contract must ban the double report, got:\n%s", sub)
	}
}

// TestTeamProfile: the squad context rides buildTeamProfile — the leader
// gets the operating protocol (CLI commands, reviewer rule) and the roster
// carries member skills; a member sees the roster + playbook; a solo goal
// gets nothing.
func TestTeamProfile(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	d.squadSvc = service.NewSquadService(st, events.NewBus())

	// A skill library entry + the leader's own selection (the roster shows
	// member skills; the leader's line carries its own).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk-1','frontend','','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE agent SET skills='["sk-1"]' WHERE id=?`, agentID); err != nil {
		t.Fatal(err)
	}
	// The goal becomes squad-assigned; the squad has the leader + coder.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO squad (id,name,leader_id,instructions,created_at) VALUES ('sq-1','team',?, 'playbook says X', '')`, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO squad_member (id,squad_id,member_type,member_id,role,created_at) VALUES ('sm-1','sq-1','agent',?,'implementer','')`, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET assignee_type='squad', assignee_id='sq-1' WHERE id=?`, goalID); err != nil {
		t.Fatal(err)
	}

	leader := d.buildTeamProfile(ctx, goalID, agentID)
	for _, want := range []string{"Squad Operating Protocol", "agentwork subgoal create --title T --assignee <agent-id>", "REVIEWER-ONLY", "playbook says X", "skills: frontend"} {
		if !strings.Contains(leader, want) {
			t.Fatalf("the leader profile must carry %q, got:\n%s", want, leader)
		}
	}
	if strings.Contains(leader, "agentwork_create_sub_goal") {
		t.Fatalf("the leader profile must not carry MCP-era tool names, got:\n%s", leader)
	}
}

// TestWakeLineShapes: ONE unified shape — "You were mentioned by <who>
// (comment <id>):" + content; no wrapper header; the anchor is optional.
func TestWakeLineShapes(t *testing.T) {
	wl := buildWakeLine("c1", "openagent-pm", "> 你觉得这个方案怎么样？")
	if strings.Contains(wl, "## Why you were woken") || !strings.Contains(wl, "You were mentioned by openagent-pm (comment c1):") ||
		!strings.Contains(wl, "你觉得这个方案怎么样？") {
		t.Fatalf("mention wake line:\n%s", wl)
	}
	wl = buildWakeLine("", "the platform", "- 1 change(s) ready to integrate — inspect with agentwork_get_change, merge each with agentwork_integrate_change")
	if !strings.Contains(wl, "You were mentioned by the platform:") || !strings.Contains(wl, "1 change(s) ready to integrate") {
		t.Fatalf("platform wake line:\n%s", wl)
	}
	wl = buildWakeLine("rpt-1", "the platform", "Review the goal's outcome — inspect the diff and the feed.")
	if !strings.Contains(wl, "You were mentioned by the platform (comment rpt-1):") {
		t.Fatalf("review wake line:\n%s", wl)
	}
	wl = buildWakeLine("", "the user", "看看 README 有没有问题")
	if !strings.Contains(wl, "You were mentioned by the user:") {
		t.Fatalf("assignment wake line:\n%s", wl)
	}
}
