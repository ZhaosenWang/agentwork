package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// TestFullCommentFeedInjected: every comment on a goal — human AND agent
// authors, no count limit — lands in the run's prompt in time order. This is
// the collaboration-chain guarantee (DESIGN.md 决策 4-6): an agent pulled in
// by another agent's mention must see what was asked of it. The earlier
// human-only + LIMIT 5 filter broke exactly that. Regression guard.
func TestFullCommentFeedInjected(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{st: st}

	gs := service.NewGoalService(st, events.NewBus())
	rt, err := service.NewRuntimeService(st).Create(context.Background(), service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agentB, err := service.NewAgentService(st, events.NewBus()).Create(context.Background(), service.Agent{Name: "B", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent B: %v", err)
	}
	dom, err := service.NewDomainService(st, events.NewBus()).Create(context.Background(), service.Domain{Name: "d", GitURL: "https://example.com/d.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	g, err := gs.Create(context.Background(), service.Goal{Title: "g", Description: "desc", AssigneeType: "agent", AssigneeID: agentB.ID, Status: "active", DomainID: dom.ID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}

	cs := service.NewCommentService(st, events.NewBus())
	cs.SetGoalService(gs)
	cs.SetRunService(service.NewRunService(st, events.NewBus()))
	// 6 comments — past the old LIMIT 5 — alternating human/agent authors.
	contents := []struct{ authorType, authorID, content string }{
		{"human", "human-1", "create this"},
		{"agent", agentB.ID, "i can help review"},
		{"human", "human-1", "yes please"},
		{"agent", agentB.ID, "reviewing the diff now"},
		{"human", "human-1", "note the edge case"},
		{"agent", agentB.ID, "done, looks good"},
	}
	for i, c := range contents {
		if _, err := cs.Create(context.Background(), service.Comment{GoalID: g.ID, AuthorType: c.authorType, AuthorID: c.authorID, Content: c.content}); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
	}

	sec := d.commentsInjection(context.Background(), g.ID)
	if sec == "" {
		t.Fatal("commentsInjection returned empty for a goal with comments")
	}
	// Every comment, both authors, in time order.
	for _, c := range contents {
		if !strings.Contains(sec, c.content) {
			t.Fatalf("comment %q missing from injected feed:\n%s", c.content, sec)
		}
	}
	for i := 1; i < len(contents); i++ {
		prev := strings.Index(sec, contents[i-1].content)
		cur := strings.Index(sec, contents[i].content)
		if prev > cur {
			t.Fatalf("comments not in time order (%q after %q):\n%s", contents[i].content, contents[i-1].content, sec)
		}
	}
}

// TestAgentGuideTrimmedByRole (决策 6-20): the coordination guide carries
// only the sections a role can act on — an owner sees the four behaviors
// and the Change pipeline; a sub-goal implementer does not (dead text
// invites dead tool calls the permission layer then rejects).
func TestAgentGuideTrimmedByRole(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{st: st}
	ctx := context.Background()

	// One teammate so the roster renders (the sections render either way).
	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	other, err := service.NewAgentService(st, events.NewBus()).Create(ctx, service.Agent{Name: "other", RuntimeID: rt.ID})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	owner := d.buildAgentGuide(ctx, other.ID, "owner")
	for _, want := range []string{"agentwork_handoff_goal", "agentwork_create_sub_goal", "Work the Change pipeline"} {
		if !strings.Contains(owner, want) {
			t.Fatalf("the owner guide must carry %q, got:\n%s", want, owner)
		}
	}

	member := d.buildAgentGuide(ctx, other.ID, "subgoal")
	for _, banned := range []string{"agentwork_handoff_goal", "agentwork_create_sub_goal", "Work the Change pipeline"} {
		if strings.Contains(member, banned) {
			t.Fatalf("a sub-goal implementer's guide must not carry %q, got:\n%s", banned, member)
		}
	}
	if !strings.Contains(member, "Your output") || !strings.Contains(member, "OWNER integrates") {
		t.Fatalf("the member's guide must name its own output contract, got:\n%s", member)
	}

	review := d.buildAgentGuide(ctx, other.ID, "review")
	if !strings.Contains(review, "Review ONLY") || strings.Contains(review, "agentwork_create_sub_goal") {
		t.Fatalf("the reviewer's guide must be review-only, got:\n%s", review)
	}

	// The roster and lists stay in every variant (all roles read the feed).
	for _, g := range []string{member, review} {
		if !strings.Contains(g, "Team roster") || !strings.Contains(g, "agentwork_get_comments") {
			t.Fatalf("the roster/lists belong to every role, got:\n%s", g)
		}
	}
}

// TestWorktreeGuidanceFirstContact (决策 6-20): the fixed contract blocks
// go in FULL on the agent's first run for the goal; later runs of the same
// goal-session get the per-run facts plus the UNCHANGED pointer — the feed
// stays full either way (it IS the session's memory).
func TestWorktreeGuidanceFirstContact(t *testing.T) {
	full := worktreeGuidance("/tmp/runs/abc", true)
	for _, want := range []string{"Worktree root: /tmp/runs/abc", "ACCESS THE WORKTREE ONLY THROUGH THE PLATFORM'S CHANNELS", "agentwork_terminal_create"} {
		if !strings.Contains(full, want) {
			t.Fatalf("the first-contact guidance must carry %q, got:\n%s", want, full)
		}
	}

	slim := worktreeGuidance("/tmp/runs/abc", false)
	if !strings.Contains(slim, "Worktree root: /tmp/runs/abc") {
		t.Fatalf("per-run facts stay in every variant, got:\n%s", slim)
	}
	if !strings.Contains(slim, "UNCHANGED from your previous turn") {
		t.Fatalf("the later-run guidance must point at the unchanged contract, got:\n%s", slim)
	}
	for _, banned := range []string{"ACCESS THE WORKTREE ONLY THROUGH THE PLATFORM'S CHANNELS", "agentwork_terminal_create"} {
		if strings.Contains(slim, banned) {
			t.Fatalf("the slim guidance must drop %q, got:\n%s", banned, slim)
		}
	}
}
