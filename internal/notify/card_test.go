package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// TestBuildReviewCard: the approval card carries the gate reason, the
// evidence summary, the reject-reason input, and approve/reject buttons
// whose values carry {action, goal_id, run_id}.
func TestBuildReviewCard(t *testing.T) {
	raw, err := buildReviewCard(ReviewGoal{
		GoalID:   "abc123456789",
		Title:    "修复登录页",
		Reason:   "merge: 每次完成需人工审批",
		RunID:    "r123456789",
		Evidence: `{"diff_stat":" 3 files changed, +12 insertions, -8 deletions","verify":"$ go test ./...\nok","agent":"修复了布局"}`,
	})
	if err != nil {
		t.Fatalf("build card: %v", err)
	}
	var card struct {
		Header struct {
			Title struct {
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Elements []json.RawMessage `json:"elements"`
	}
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("card must be valid JSON: %v\n%s", err, raw)
	}
	if !strings.Contains(card.Header.Title.Content, "待审批") {
		t.Fatalf("card header must announce the checkpoint, got %q", card.Header.Title.Content)
	}
	var actions []map[string]any
	var hasInput bool
	for _, el := range card.Elements {
		var e map[string]any
		if err := json.Unmarshal(el, &e); err != nil {
			continue
		}
		if e["tag"] == "input" {
			hasInput = true
		}
		if e["tag"] == "action" {
			actions = append(actions, e)
		}
	}
	if !hasInput {
		t.Fatal("card must carry the reject-reason input")
	}
	if len(actions) != 1 {
		t.Fatalf("expected one action element, got %d", len(actions))
	}
	btns, _ := actions[0]["actions"].([]any)
	if len(btns) != 2 {
		t.Fatalf("expected 2 buttons, got %d", len(btns))
	}
	for _, b := range btns {
		btn := b.(map[string]any)
		value, _ := btn["value"].(map[string]any)
		if value["goal_id"] != "abc123456789" || value["run_id"] != "r123456789" {
			t.Fatalf("button value must carry goal_id + run_id: %v", value)
		}
		if value["action"] != "approve" && value["action"] != "reject" {
			t.Fatalf("button action must be approve|reject, got %v", value["action"])
		}
	}
	if !strings.Contains(raw, "修复登录页") || !strings.Contains(raw, "+12") {
		t.Fatal("card body must carry title and diff stat")
	}
}

// TestEvidenceSummary: the run evidence JSON renders into compact card
// markdown; unknown shapes degrade to "" (never a broken card).
func TestEvidenceSummary(t *testing.T) {
	s := evidenceSummary(`{"diff_stat":" 1 file changed, +2 insertions","changed":["a.go"],"verify":"$ go test ./...\nok 0.1s","agent":"x"}`)
	if !strings.Contains(s, "1 file changed") || !strings.Contains(s, "go test") {
		t.Fatalf("evidence summary incomplete: %s", s)
	}
	if evidenceSummary("not json") != "" {
		t.Fatal("bad evidence must degrade to empty")
	}
	if evidenceSummary("") != "" {
		t.Fatal("empty evidence must degrade to empty")
	}
}

// TestBuildProcessedCard: the post-decision card stamps the outcome and
// carries no buttons.
func TestBuildProcessedCard(t *testing.T) {
	raw, err := buildProcessedCard("abc123456789", "approve")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "已批准") {
		t.Fatalf("processed card must stamp the decision: %s", raw)
	}
}

// TestBuildDigestCard: the digest aggregates pending approvals, yesterday's
// completions, and failures from the store.
func TestBuildDigestCard(t *testing.T) {
	st, err := store.Open(":memory:") // in-memory
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gs := service.NewGoalService(st, events.NewBus())
	ds := service.NewDomainService(st, events.NewBus())
	dom, err := ds.Create(ctx, service.Domain{Name: "d", GitURL: "https://e.com/d.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.FreezeChecks(ctx, dom.ID, service.Checks{
		Gates: []service.GateRule{{Name: "merge", When: "必审"}},
	}, "strong"); err != nil {
		t.Fatal(err)
	}
	// run rows reference agent; agent references runtime — seed both.
	seedAgentRow(t, ctx, st)
	// A goal parked in review + one done.
	reviewing, err := gs.Create(ctx, service.Goal{Title: "待审任务", DomainID: dom.ID, AssigneeType: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='review', review_request='merge: 必审' WHERE id=?`, reviewing.ID); err != nil {
		t.Fatal(err)
	}
	done, err := gs.Create(ctx, service.Goal{Title: "昨日完成", DomainID: dom.ID, AssigneeType: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='done' WHERE id=?`, done.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,status,finished_at,queued_at,created_at) VALUES (?,?,?,?,?,?,?)`,
		"r1", done.ID, "a1", "completed", dayStart.Add(-2*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	qs := NewSQLQueryStore(st)
	raw, err := BuildDigestCard(ctx, qs, dayStart.Add(-24*time.Hour), dayStart, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "待审任务") || !strings.Contains(raw, "昨日完成") {
		t.Fatalf("digest must carry pending + completed: %s", raw)
	}
}

// TestIntakeBuildPrompt: the parser prompt carries the roster and the
// intake.json contract; unconfigured parser agent is surfaced to the owner.
func TestIntakeBuildPrompt(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runSvc := service.NewRunService(st, events.NewBus())
	seedAgentRow(t, ctx, st)
	qs := NewSQLQueryStore(st)
	fake := &mapSettings{vals: map[string]string{}}
	is := NewIntakeService(qs, fake, runSvc)
	prompt, err := is.BuildPrompt(ctx, "创建任务：把登录页修一下")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "worker1") || !strings.Contains(prompt, "intake.json") {
		t.Fatalf("prompt must carry roster + contract: %s", prompt)
	}
	// Unconfigured parser agent → Enqueue surfaces the setup hint.
	if _, err := is.Enqueue(ctx, "msg1", prompt); err == nil || !strings.Contains(err.Error(), "解析 agent") {
		t.Fatalf("expected unconfigured-agent error, got %v", err)
	}
}

// seedAgentRow inserts a runtime + agent pair (FK chain for run rows).
func seedAgentRow(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,created_at) VALUES ('rt1','rt1',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a1','worker1','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// mapSettings is a SettingsStore fake for tests.
type mapSettings struct {
	vals map[string]string
}

func (m *mapSettings) Get(_ context.Context, key string) (string, error) { return m.vals[key], nil }
func (m *mapSettings) Set(_ context.Context, key, value string) error    { m.vals[key] = value; return nil }
func (m *mapSettings) Delete(_ context.Context, key string) error        { delete(m.vals, key); return nil }
