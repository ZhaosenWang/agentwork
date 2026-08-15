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
	rt, err := service.NewRuntimeService(st).Create(context.Background(), service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
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
// language, the solo team line renders without a squad.
func TestFixedBlockShape(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	block := d.buildFixedBlock(ctx, goalID, agentID, "B", "the implementer", "sys", "owner", "g", "测试能过", "repo", "", "/tmp/wt")

	for _, want := range []string{
		"# Background & Requirements", "# Goal", "# Team", "# Who You Are", "# Tools",
		"- Title: g", "- Acceptance policy: 测试能过",
		"Working solo — no team on this goal.",
		"B — the implementer",
		"agentwork_get_comments", "agentwork_terminal_create",
		"WITHOUT `after`", // the no-memory contract
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("fixed block must carry %q, got:\n%s", want, block)
		}
	}
	_ = st
}

// TestFixedBlockRoleContracts: each role gets its own behavioral contract —
// the reviewer is told review-only, the owner the dispatch rules.
func TestFixedBlockRoleContracts(t *testing.T) {
	d, _, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	owner := d.buildFixedBlock(ctx, goalID, agentID, "B", "", "", "owner", "g", "", "repo", "", "/tmp/wt")
	for _, want := range []string{"final message becomes your run's report", "never write ids", "JUDGED, not declared"} {
		if !strings.Contains(owner, want) {
			t.Fatalf("the owner contract must carry %q", want)
		}
	}
	reviewer := d.buildFixedBlock(ctx, goalID, agentID, "B", "", "", "review", "g", "", "repo", "", "/tmp/wt")
	if !strings.Contains(reviewer, "REVIEW ONLY") || !strings.Contains(reviewer, "never do the work") {
		t.Fatalf("the reviewer contract must be review-only, got:\n%s", reviewer)
	}
	sub := d.buildFixedBlock(ctx, goalID, agentID, "B", "", "", "subgoal", "g", "", "repo", "", "/tmp/wt")
	if !strings.Contains(sub, "NEVER post your conclusions with agentwork_comment_goal") {
		t.Fatalf("the subgoal contract must ban the double report, got:\n%s", sub)
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
