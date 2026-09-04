package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// digestDaemonFixture builds a Daemon struct over a fresh store with the
// marker set to the given schedule id ('' = unset) — enough for the digest
// helpers, which only touch app_settings / goal / run / agent / runtime /
// machine.
func digestDaemonFixture(t *testing.T, markerScheduleID string) (*Daemon, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "aw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc}
	if markerScheduleID != "" {
		if _, err := st.DB().Exec(
			`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,'2026-01-01T00:00:00Z')`,
			service.DigestKeySchedule, `"`+markerScheduleID+`"`); err != nil {
			t.Fatalf("set marker: %v", err)
		}
	}
	return d, st
}

// insertDigestAgents plants machine+runtime+agent rows: steward first (when
// withSteward), then one standard agent — availability is controlled per
// row (machine connected / runtime status). Call once per test; per-row
// tweaks afterwards go through direct SQL (e.g. UPDATE runtime SET status).
func insertDigestAgents(t *testing.T, st *store.Store, withSteward, stewardAvailable, standardAvailable bool) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	mk := func(mid, status string) {
		if _, err := st.DB().Exec(
			`INSERT INTO machine (id,name,status,created_at) VALUES (?,?,?,?)`, mid, mid, status, ts); err != nil {
			t.Fatalf("insert machine: %v", err)
		}
	}
	if withSteward {
		mk("m-steward", map[bool]string{true: "connected", false: "offline"}[stewardAvailable])
		if _, err := st.DB().Exec(
			`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-steward','claude@m-steward','m-steward','[]','{}','active',?)`, ts); err != nil {
			t.Fatalf("insert runtime: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-steward','AI SHELL','steward','rt-steward',?)`, ts); err != nil {
			t.Fatalf("insert steward: %v", err)
		}
	}
	mk("m-std", map[bool]string{true: "connected", false: "offline"}[standardAvailable])
	if _, err := st.DB().Exec(
		`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-std','claude@m-std','m-std','[]','{}','active',?)`, ts); err != nil {
		t.Fatalf("insert runtime: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-std','worker','standard','rt-std',?)`, ts); err != nil {
		t.Fatalf("insert standard: %v", err)
	}
}

func TestPickDigestExecutorStewardFirst(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	insertDigestAgents(t, st, true, true, true)
	agentID, steward, ok := d.pickDigestExecutor(context.Background())
	if !ok || !steward || agentID != "a-steward" {
		t.Fatalf("pick = (%q,%v,%v), want steward a-steward", agentID, steward, ok)
	}
}

func TestPickDigestExecutorFallsBackToStandard(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	// Steward's machine offline → unavailable; standard available.
	insertDigestAgents(t, st, true, false, true)
	agentID, steward, ok := d.pickDigestExecutor(context.Background())
	if !ok || steward || agentID != "a-std" {
		t.Fatalf("pick = (%q,%v,%v), want fallback a-std", agentID, steward, ok)
	}
	// Steward runtime absent → same fallback (no new fixture rows).
	if _, err := st.DB().Exec(`UPDATE runtime SET status='absent' WHERE id='rt-steward'`); err != nil {
		t.Fatalf("absent steward runtime: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE machine SET status='connected' WHERE id='m-steward'`); err != nil {
		t.Fatalf("reconnect steward machine: %v", err)
	}
	agentID, steward, ok = d.pickDigestExecutor(context.Background())
	if !ok || steward || agentID != "a-std" {
		t.Fatalf("absent-runtime pick = (%q,%v,%v), want fallback a-std", agentID, steward, ok)
	}
}

func TestPickDigestExecutorNoneAvailable(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	insertDigestAgents(t, st, true, false, false)
	if _, _, ok := d.pickDigestExecutor(context.Background()); ok {
		t.Fatalf("pick succeeded, want ok=false when every machine is offline")
	}
}

// insertDigestGoal plants a system-created goal (the digest firing shape)
// plus its run. The executor agent must exist (run.agent_id FK) — a plain
// standard agent row is planted if none is present yet.
func insertDigestGoal(t *testing.T, st *store.Store, goalID, scheduleID, runID string) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM agent WHERE id='a-steward'`).Scan(&n)
	if n == 0 {
		// run.agent_id → agent.runtime_id → runtime.machine_id are all FKs —
		// plant a minimal valid chain for the executor.
		if _, err := st.DB().Exec(
			`INSERT INTO machine (id,name,status,created_at) VALUES ('m-x','m-x','connected',?)`, ts); err != nil {
			t.Fatalf("insert machine: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-x','claude@m-x','m-x','[]','{}','active',?)`, ts); err != nil {
			t.Fatalf("insert runtime: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-steward','AI SHELL','steward','rt-x',?)`, ts); err != nil {
			t.Fatalf("insert executor agent: %v", err)
		}
	}
	if _, err := st.DB().Exec(
		`INSERT INTO goal (id,title,domain_id,assignee_type,assignee_id,status,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		goalID, "digest goal", "dom-1", "agent", "a-steward", "active", "system", scheduleID, ts); err != nil {
		t.Fatalf("insert goal: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO run (id,goal_id,agent_id,status,role,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		runID, goalID, "a-steward", "running", "owner", 1, ts, ts); err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

func TestDigestGoalIdentification(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO domain (id,type,name,created_at) VALUES ('dom-1','scratch','AI知识精选',?)`, ts); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	insertDigestGoal(t, st, "g-digest", "sched-1", "r-digest")
	insertDigestGoal(t, st, "g-other", "sched-other", "r-other")

	if got := d.digestGoalIDForGoal(context.Background(), "g-digest"); got != "g-digest" {
		t.Fatalf("digest goal not recognized: %q", got)
	}
	if got := d.digestGoalIDForGoal(context.Background(), "g-other"); got != "" {
		t.Fatalf("non-digest goal misrecognized: %q", got)
	}
	if got := d.digestGoalIDForRun(context.Background(), "r-digest"); got != "g-digest" {
		t.Fatalf("digest run not recognized: %q", got)
	}
	if got := d.digestGoalIDForRun(context.Background(), "r-other"); got != "" {
		t.Fatalf("non-digest run misrecognized: %q", got)
	}
}

func TestCollectDigestBatchWritesAndIsIdempotent(t *testing.T) {
	// Isolate HOME — digestRoot() lands in the temp home.
	t.Setenv("HOME", t.TempDir())
	d, st := digestDaemonFixture(t, "sched-1")
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO domain (id,type,name,created_at) VALUES ('dom-1','scratch','AI知识精选',?)`, ts); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	insertDigestGoal(t, st, "g-1", "sched-1", "r-1")

	manifest := []service.DigestManifestItem{
		{Title: "动态一", Summary: "摘要一", File: "1.md"},
		{Title: "动态二", Summary: "摘要二", File: "2.md"},
	}
	mb, _ := json.Marshal(manifest)
	artifacts := map[string]string{
		"manifest.json": string(mb),
		"1.md":          "# 动态一\n正文",
		"2.md":          "# 动态二\n正文",
	}
	d.collectDigestBatch(context.Background(), "g-1", artifacts)

	// articles.json has both entries, newest first, absolute paths.
	root := digestRoot()
	b, err := os.ReadFile(filepath.Join(root, "articles.json"))
	if err != nil {
		t.Fatalf("read articles.json: %v", err)
	}
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil {
		t.Fatalf("parse articles.json: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("articles = %d, want 2", len(articles))
	}
	for i, a := range articles {
		if a.Title == "" || a.Summary == "" || a.Path == "" || a.CreateTime == "" {
			t.Fatalf("entry %d incomplete: %+v", i, a)
		}
		if !filepath.IsAbs(a.Path) {
			t.Fatalf("entry %d path not absolute: %q", i, a.Path)
		}
		if _, err := os.Stat(a.Path); err != nil {
			t.Fatalf("entry %d file missing: %v", i, err)
		}
	}
	// Batch dir exists with the md files.
	if _, err := os.Stat(filepath.Join(root, "g-1", "1.md")); err != nil {
		t.Fatalf("batch file missing: %v", err)
	}

	// Idempotent re-collect: same goal → no duplicate entries.
	d.collectDigestBatch(context.Background(), "g-1", artifacts)
	b2, _ := os.ReadFile(filepath.Join(root, "articles.json"))
	var articles2 []service.DigestArticle
	_ = json.Unmarshal(b2, &articles2)
	if len(articles2) != 2 {
		t.Fatalf("re-collect duplicated entries: %d", len(articles2))
	}
}

func TestCollectDigestBatchCapsAndPrunes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	root := digestRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fill the json exactly to the cap: 199 single-article "old-solo"
	// entries plus a "g-a" entry that lands LAST — the next collect pushes
	// the json to cap+1 and evicts exactly the g-a entry, orphaning its
	// directory.
	stale := make([]service.DigestArticle, service.DigestMaxArticles-1)
	for i := range stale {
		stale[i] = service.DigestArticle{Title: "old", Path: filepath.Join(root, "old-solo", "1.md"), CreateTime: "2026-01-01T00:00:00Z"}
	}
	stale = append(stale, service.DigestArticle{Title: "a", Path: filepath.Join(root, "g-a", "1.md"), CreateTime: "2026-01-01T00:00:00Z"})
	if err := writeDigestArticles(root, stale); err != nil {
		t.Fatalf("seed stale json: %v", err)
	}
	for _, batch := range []string{"old-solo", "g-a"} {
		if err := os.MkdirAll(filepath.Join(root, batch), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, batch, "1.md"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d.collectDigestBatch(context.Background(), "g-new", map[string]string{
		"manifest.json": `[{"title":"新动态","summary":"新摘要","file":"1.md"}]`,
		"1.md":          "# 新动态",
	})
	b, _ := os.ReadFile(filepath.Join(root, "articles.json"))
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(articles) != service.DigestMaxArticles {
		t.Fatalf("articles = %d, want capped %d", len(articles), service.DigestMaxArticles)
	}
	if articles[0].Title != "新动态" {
		t.Fatalf("newest-first violated: entry 0 = %+v", articles[0])
	}
	// The cap-evicted batch directory is pruned; the survivors stay.
	if _, err := os.Stat(filepath.Join(root, "g-a")); !os.IsNotExist(err) {
		t.Fatalf("evicted batch not pruned (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "old-solo")); err != nil {
		t.Fatalf("surviving batch wrongly pruned: %v", err)
	}
}

func TestCollectDigestBatchToleratesCorruptJson(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	root := digestRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "articles.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.collectDigestBatch(context.Background(), "g-1", map[string]string{
		"manifest.json": `[{"title":"t","summary":"s","file":"1.md"}]`,
		"1.md":          "content",
	})
	b, err := os.ReadFile(filepath.Join(root, "articles.json"))
	if err != nil {
		t.Fatalf("articles.json not rebuilt: %v", err)
	}
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil || len(articles) != 1 {
		t.Fatalf("corrupt json not reset (err=%v, n=%d)", err, len(articles))
	}
	if _, err := os.Stat(filepath.Join(root, "articles.json.corrupt")); err != nil {
		t.Fatalf("corrupt backup missing: %v", err)
	}
}

func TestCollectDigestBatchWithoutManifestFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	d.collectDigestBatch(context.Background(), "g-1", map[string]string{
		"1.md": "orphan content",
	})
	// No manifest → fixed-name fallback: the md file is collected with the
	// filename as the (empty metadata) entry.
	var articles []service.DigestArticle
	b, err := os.ReadFile(filepath.Join(digestRoot(), "articles.json"))
	if err != nil {
		t.Fatalf("no articles.json: %v", err)
	}
	_ = json.Unmarshal(b, &articles)
	if len(articles) != 1 {
		t.Fatalf("fallback articles = %d, want 1", len(articles))
	}
	if _, err := os.Stat(filepath.Join(digestRoot(), "g-1", "1.md")); err != nil {
		t.Fatalf("fallback file missing: %v", err)
	}
}
