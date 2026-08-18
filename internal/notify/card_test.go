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
// evidence summary, and approve/reject buttons whose values carry
// {action, goal_id, run_id}. Schema 2.0: body.elements with markdown
// and column_set button row (no input — removed).
func TestBuildReviewCard(t *testing.T) {
	raw, err := buildReviewCard(ReviewGoal{
		GoalID:   "abc123456789",
		Title:    "修复登录页",
		Reason:   "merge: 每次完成需人工审批",
		RunID:    "r123456789",
		Evidence: `{"diff_stat":" 3 files changed, +12 insertions, -8 deletions","verify":"$ go test ./...\nok","agent":"修复了布局"}`,
	}, nil)
	if err != nil {
		t.Fatalf("build card: %v", err)
	}
	var c struct {
		Schema string `json:"schema"`
		Header struct {
			Title struct {
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Body struct {
			Elements []json.RawMessage `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("card must be valid JSON: %v\n%s", err, raw)
	}
	if c.Schema != "2.0" {
		t.Fatalf("expected schema 2.0, got %q", c.Schema)
	}
	if !strings.Contains(c.Header.Title.Content, "待审批") {
		t.Fatalf("card header must announce the checkpoint, got %q", c.Header.Title.Content)
	}
	var buttonRow map[string]any
	var hasInput, hasMarkdown bool
	for _, el := range c.Body.Elements {
		var e map[string]any
		if err := json.Unmarshal(el, &e); err != nil {
			continue
		}
		switch e["tag"] {
		case "input":
			hasInput = true
		case "markdown":
			hasMarkdown = true
		case "column_set":
			buttonRow = e
		}
	}
	if !hasMarkdown {
		t.Fatal("card body must have a markdown element")
	}
	if hasInput {
		t.Fatal("card must NOT carry an input element (removed — the send button had no effect)")
	}
	if buttonRow == nil {
		t.Fatal("card must carry a column_set button row")
	}
	cols, _ := buttonRow["columns"].([]any)
	if len(cols) != 2 {
		t.Fatalf("expected 2 button columns, got %d", len(cols))
	}
	for _, col := range cols {
		colMap := col.(map[string]any)
		elements, _ := colMap["elements"].([]any)
		btn := elements[0].(map[string]any)
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
	if !strings.Contains(s, "agent 自述") {
		t.Fatalf("agent section header missing: %s", s)
	}
	if evidenceSummary("not json") != "" {
		t.Fatal("bad evidence must degrade to empty")
	}
	if evidenceSummary("") != "" {
		t.Fatal("empty evidence must degrade to empty")
	}
}

// TestEvidenceSummaryGuards: the guards field renders when present.
func TestEvidenceSummaryGuards(t *testing.T) {
	s := evidenceSummary(`{"diff_stat":"","changed":["a.go"],"guards":"no_todo: ok","agent":"done"}`)
	if !strings.Contains(s, "约束：no_todo: ok") {
		t.Fatalf("guards must render: %s", s)
	}
}

// TestEvidenceSummaryAgentTruncation: a long multi-paragraph agent report is
// cut on a paragraph boundary (\n\n), not mid-bullet — the truncated output
// ends with "…" and contains no broken bullet fragments.
func TestEvidenceSummaryAgentTruncation(t *testing.T) {
	// Each paragraph ~300 bytes; 6 paragraphs = ~1800 bytes, over the 1200 cap.
	long := strings.Repeat("这是第一段很长的 agent 报告内容用来测试段落截断逻辑。", 8) + "\n\n" +
		"第二段：简短。\n\n" +
		"- bullet 1\n- bullet 2\n- bullet 3\n\n" +
		strings.Repeat("第四段也很长用来确保累积超限。", 20) + "\n\n" +
		"第五段不应该出现。"
	s := evidenceSummary(`{"diff_stat":"1 file","agent":` + jsonQuote(long) + `}`)
	if !strings.Contains(s, "…") {
		t.Fatalf("truncated agent must end with …: %s", s)
	}
	if strings.Contains(s, "第五段") {
		t.Fatalf("content past the cap must not appear: %s", s)
	}
	// The bullet list paragraph must be intact (all 3 bullets) or absent —
	// never a partial cut mid-list.
	if strings.Contains(s, "bullet 1") && !strings.Contains(s, "bullet 3") {
		t.Fatalf("bullet list was split mid-way: %s", s)
	}
}

// jsonQuote escapes a string into a JSON string literal (for test fixtures).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestRenderReviewComment: a JSON evidence blob is parsed and its agent field
// rendered as markdown; a plain-text comment keeps the > quote.
func TestRenderReviewComment(t *testing.T) {
	// JSON with agent field — extracted, no > quote, no raw JSON.
	jsonComment := `{"agent":"所有编码步骤完成，Go 测试通过。\n\n后端改动：\n- goal.go 新增 filter","diff_stat":"12 files","changed":["a.go"]}`
	got := renderReviewComment(jsonComment)
	if strings.HasPrefix(got, ">") {
		t.Fatalf("JSON comment with agent must not be quoted: %s", got)
	}
	if !strings.Contains(got, "所有编码步骤完成") {
		t.Fatalf("agent text must be extracted: %s", got)
	}
	if strings.Contains(got, "diff_stat") {
		t.Fatalf("raw JSON fields must not leak: %s", got)
	}

	// JSON without agent field — falls back to quote.
	got = renderReviewComment(`{"diff_stat":"12 files"}`)
	if !strings.HasPrefix(got, ">") {
		t.Fatalf("JSON without agent must fall back to quote: %s", got)
	}

	// Plain text — quoted as before.
	got = renderReviewComment("实现合理，可以批准。")
	if got != "> 实现合理，可以批准。" {
		t.Fatalf("plain text must be quoted: %s", got)
	}

	// Empty — empty.
	if renderReviewComment("") != "" {
		t.Fatal("empty comment must return empty")
	}

	// Long agent text — truncated on paragraph boundary.
	long := strings.Repeat("这是很长的一段审查意见内容。", 80)
	got = renderReviewComment(`{"agent":` + jsonQuote(long) + `}`)
	if !strings.Contains(got, "…") {
		t.Fatalf("long agent text must be truncated: %s", got[:50])
	}
}

// TestBuildProcessedCard: the post-decision card stamps the outcome and
// carries no buttons. Schema 2.0 structure.
func TestBuildProcessedCard(t *testing.T) {
	raw, err := buildProcessedCard("abc123456789", "approve", false)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Schema string `json:"schema"`
		Header struct {
			Template string `json:"template"`
		} `json:"header"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if c.Schema != "2.0" {
		t.Fatalf("expected schema 2.0, got %q", c.Schema)
	}
	if c.Header.Template != "green" {
		t.Fatalf("approved card should be green, got %q", c.Header.Template)
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
	var c struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if c.Schema != "2.0" {
		t.Fatalf("expected schema 2.0, got %q", c.Schema)
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
	if _, err := is.Enqueue(ctx, "msg1", prompt); err == nil || !strings.Contains(err.Error(), "IM 解析 Agent 未配置或者已删除") {
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

// TestIntakeStaleAgentConfig: a platform.m3.intake_agent that points at an
// agent row that no longer exists (deleted, or DB re-seeded) must surface a
// human-readable setup hint at Enqueue — NOT an opaque FOREIGN KEY
// constraint failure from EnqueueProcessorRun's INSERT. This is the setup
// drift the live system hit ("解析任务创建失败：FOREIGN KEY constraint failed").
func TestIntakeStaleAgentConfig(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runSvc := service.NewRunService(st, events.NewBus())
	seedAgentRow(t, ctx, st) // seeds 'a1'/'worker1'
	qs := NewSQLQueryStore(st)
	fake := &mapSettings{vals: map[string]string{}}
	// Configure a parser agent id that does NOT exist in the agent table.
	if err := fake.Set(ctx, "platform.m3", `{"intake_agent":"deadbeefdeadbeefdeadbeefdeadbeef"}`); err != nil {
		t.Fatal(err)
	}
	is := NewIntakeService(qs, fake, runSvc)
	_, err = is.Enqueue(ctx, "msg1", "prompt")
	if err == nil {
		t.Fatal("stale intake_agent must fail Enqueue")
	}
	if !strings.Contains(err.Error(), "IM 解析 Agent 未配置或者已删除") {
		t.Fatalf("stale config must surface the setup hint, got %q", err.Error())
	}
	// And a live config must NOT fail at the existence check (it reaches
	// EnqueueProcessorRun, which enqueues a real run row).
	if err := fake.Set(ctx, "platform.m3", `{"intake_agent":"a1"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := is.Enqueue(ctx, "msg1", "prompt"); err != nil {
		t.Fatalf("live intake_agent must enqueue, got %v", err)
	}
	// The run row is platform-level: domain_id is "" (not the msgID).
	var domainID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT domain_id FROM run WHERE run_kind='processor' AND run_type='intake'`).Scan(&domainID); err != nil {
		t.Fatalf("query intake run: %v", err)
	}
	if domainID != "" {
		t.Fatalf("intake run domain_id must be empty (platform-level), got %q", domainID)
	}
}

// mapSettings is a SettingsStore fake for tests.
type mapSettings struct {
	vals map[string]string
}

func (m *mapSettings) Get(_ context.Context, key string) (string, error) { return m.vals[key], nil }
func (m *mapSettings) Set(_ context.Context, key, value string) error {
	m.vals[key] = value
	return nil
}
func (m *mapSettings) Delete(_ context.Context, key string) error { delete(m.vals, key); return nil }

// TestIntakeDraftClarification: the multi-domain clarification — a pending
// draft surfaces in the next parser prompt (so a bare repo name completes the
// pending task), expires after TTL, and clears.
func TestIntakeDraftClarification(t *testing.T) {
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

	// No draft → no clarification context.
	p, err := is.BuildPrompt(ctx, "创建任务：修一下")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "补全之前的一个创建请求") {
		t.Fatal("no draft must not inject clarification context")
	}
	// Save a goal-kind draft (title known, domain missing) → the next prompt
	// carries the known field + the missing field under the generalized
	// "补全之前的一个创建请求" framing.
	if err := is.SaveDraft(ctx, IntakeDraft{
		Kind: "goal", Payload: `{"title":"修一下","assignee_id":"a1"}`,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	p, err = is.BuildPrompt(ctx, "test-repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "补全之前的一个创建请求") || !strings.Contains(p, "修一下") {
		t.Fatalf("draft context must be injected: %s", p)
	}
	if !strings.Contains(p, "项目/仓库") {
		t.Fatalf("draft context must list the missing domain field: %s", p)
	}
	if err := is.ClearDraft(ctx); err != nil {
		t.Fatal(err)
	}
	// Cleared → gone.
	if _, ok := is.LoadDraft(ctx); ok {
		t.Fatal("cleared draft must be gone")
	}
	// Expired draft → treated as absent.
	if err := is.SaveDraft(ctx, IntakeDraft{
		Kind: "goal", Payload: `{"title":"旧任务"}`,
		CreatedAt: time.Now().Add(-30 * time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := is.LoadDraft(ctx); ok {
		t.Fatal("expired draft must be treated as absent")
	}
}

// TestIntakeDraftAgentKind: an agent-kind draft surfaces in the next parser
// prompt under the generalized completion framing — the known agent fields
// (name/runtime) are listed as "已知字段", the still-missing bits (skills when
// unspecified) as "缺失字段". The goal-kind draft uses the same framing
// (regression: both kinds share one generalized block, no per-kind special
// case).
func TestIntakeDraftAgentKind(t *testing.T) {
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

	// No agent draft → no completion context.
	p, err := is.BuildPrompt(ctx, "git-helper")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "补全之前的一个创建请求") {
		t.Fatalf("no agent draft must not inject completion context: %s", p)
	}

	// Save an agent-kind draft (name+runtime known, skills unspecified) → the
	// next prompt carries the known fields + lists skills as missing.
	if err := is.SaveDraft(ctx, IntakeDraft{
		Kind: "agent",
		Payload: `{"name":"代码审查","runtime_id":"rt1","description":"审查 PR","system_prompt":"你是审查员","skills":[],"skills_specified":false}`,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	p, err = is.BuildPrompt(ctx, "git-helper test-runner")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"补全之前的一个创建请求", "代码审查", "rt1", "skills",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("agent draft context must carry %q: %s", want, p)
		}
	}

	// A goal-kind draft uses the SAME generalized framing (regression guard —
	// no per-kind special case leaks).
	if err := is.ClearDraft(ctx); err != nil {
		t.Fatal(err)
	}
	if err := is.SaveDraft(ctx, IntakeDraft{
		Kind: "goal", Payload: `{"title":"修一下","assignee_id":"a1"}`,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	p, err = is.BuildPrompt(ctx, "test-repo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "补全之前的一个创建请求") || !strings.Contains(p, "create_goal") {
		t.Fatalf("goal-kind draft must use the same generalized framing: %s", p)
	}
}
