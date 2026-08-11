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
