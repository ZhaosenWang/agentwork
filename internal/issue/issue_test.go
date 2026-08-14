package issue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// mockGitHub serves canned API responses and records what was called.
func mockGitHub(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 1, "title": "登录页样式错位", "body": "侧边栏溢出", "html_url": "https://github.com/x/y/issues/1"},
				{"number": 2, "title": "PR 不是 issue", "body": "", "html_url": "https://github.com/x/y/pull/2",
					"pull_request": map[string]any{"url": "https://api.github.com/repos/x/y/pulls/2"}},
			})
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "body": "注意保留旧接口兼容", "user": map[string]any{"login": "eushing"}, "created_at": time.Now()},
			})
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost:
			var cm struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&cm)
			calls = append(calls, "BODY: "+cm.Body)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	return srv, &calls
}

func TestListOpenIssuesExcludesPRs(t *testing.T) {
	srv, _ := mockGitHub(t)
	defer srv.Close()
	c := &Client{token: "t", http: srv.Client(), baseURL: srv.URL}
	issues, err := c.ListOpenIssues(context.Background(), "x/y")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("PR must be excluded, got %+v", issues)
	}
}

// TestParseSourceRef covers the ref encoding round-trip.
func TestParseSourceRef(t *testing.T) {
	provider, repo, num, ok := ParseSourceRef("github:yusheng-g/agentwork#42")
	if !ok || provider != "github" || repo != "yusheng-g/agentwork" || num != 42 {
		t.Fatalf("parse: %q %d %v", repo, num, ok)
	}
	if _, _, _, ok := ParseSourceRef("github:x/y"); ok {
		t.Fatal("missing #number must fail")
	}
	if _, _, _, ok := ParseSourceRef("im://other"); ok {
		t.Fatal("non-github ref must fail")
	}
}

// TestPollerCreatesGoalsIdempotently: the poller turns open issues (PRs
// excluded) into goals ONCE — a second poll creates nothing (unique
// source_ref).
func TestPollerCreatesGoalsIdempotently(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bus := events.NewBus()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,created_at) VALUES ('rt1','rt1',?)`, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a1','worker','rt1',1,?)`, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ds := service.NewDomainService(st, bus)
	if _, err := ds.Create(ctx, service.Domain{
		Name: "d1", GitURL: "https://e.com/d1.git",
		IssueRepo: "x/y", IssueAssignee: "a1", GitCredentials: "tok",
	}); err != nil {
		t.Fatal(err)
	}
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)

	srv, _ := mockGitHub(t)
	defer srv.Close()
	p := NewPoller(st, goalSvc, runSvc)
	p.newProvider = func(provider, token string) (Provider, error) {
		c := NewClient(token)
		c.http = srv.Client()
		c.baseURL = srv.URL
		return c, nil
	}

	n, err := p.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 goal (PR excluded), got %d", n)
	}
	// The created goal is issue-sourced and active.
	goals, err := goalSvc.List(ctx)
	if err != nil || len(goals) != 1 {
		t.Fatalf("goals: %d %v", len(goals), err)
	}
	if goals[0].SourceRef != "github:x/y#1" || goals[0].Status != "active" {
		t.Fatalf("goal must carry the issue ref and be active: %+v", goals[0])
	}
	// Idempotent: second poll creates nothing.
	n, err = p.Poll(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second poll must be a no-op: n=%d err=%v", n, err)
	}
}

// TestCloserClosesIssueOnDelivered: a delivered issue-sourced goal closes its
// issue and comments; a non-issue goal is untouched.
func TestCloserClosesIssueOnDelivered(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bus := events.NewBus()
	ds := service.NewDomainService(st, bus)
	dom, err := ds.Create(ctx, service.Domain{Name: "d1", GitURL: "https://e.com/d1.git", GitCredentials: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	gs := service.NewGoalService(st, bus)
	issueGoal, err := gs.Create(ctx, service.Goal{
		Title: "x", DomainID: dom.ID, AssigneeType: "human",
		SourceRef: "github:x/y#7",
	})
	if err != nil {
		t.Fatal(err)
	}
	plainGoal, err := gs.Create(ctx, service.Goal{Title: "p", DomainID: dom.ID, AssigneeType: "human"})
	if err != nil {
		t.Fatal(err)
	}

	srv, calls := mockGitHub(t)
	defer srv.Close()
	c := NewCloser(st)
	c.newProvider = func(provider, token string) (Provider, error) {
		cl := NewClient(token)
		cl.http = srv.Client()
		cl.baseURL = srv.URL
		return cl, nil
	}
	c.OnDelivered(ctx, issueGoal.ID, "merged feat-x → main", []string{"77cfd90b95bbe2cd8ae4393b470d679b528226b3 docs: 添加 README"})
	c.OnDelivered(ctx, plainGoal.ID, "", nil)

	joined := strings.Join(*calls, " ")
	if !strings.Contains(joined, "PATCH /repos/x/y/issues/7") {
		t.Fatalf("issue goal must close its issue, calls: %s", joined)
	}
	if !strings.Contains(joined, "POST /repos/x/y/issues/7/comments") {
		t.Fatalf("issue goal must comment on close, calls: %s", joined)
	}
	if !strings.Contains(joined, "77cfd90b docs: 添加 README") || !strings.Contains(joined, "https://github.com/x/y/commit/77cfd90b95bbe2cd8ae4393b470d679b528226b3") {
		t.Fatalf("close comment must carry a clickable fix commit link, calls: %s", joined)
	}
}

// TestVerifySignature: correct secret verifies, wrong secret and missing
// signature fail (constant-time compare).
func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	good := "sha256=" + hmacSHA256Hex("s3cret", body)
	if !verifySignature("s3cret", body, good) {
		t.Fatal("correct secret must verify")
	}
	if verifySignature("wrong", body, good) {
		t.Fatal("wrong secret must fail")
	}
	if verifySignature("s3cret", body, "") {
		t.Fatal("missing signature must fail")
	}
}

// TestWebhookHandle: an opened-issue delivery with a valid signature creates
// the goal; bad signatures, non-tracking repos, and non-opened actions no-op.
func TestWebhookHandle(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bus := events.NewBus()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,created_at) VALUES ('rt1','rt1',?)`, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a1','worker','rt1',1,?)`, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ds := service.NewDomainService(st, bus)
	if _, err := ds.Create(ctx, service.Domain{
		Name: "d1", GitURL: "https://e.com/d1.git",
		IssueRepo: "x/y", IssueAssignee: "a1", GitCredentials: "tok",
	}); err != nil {
		t.Fatal(err)
	}
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	p := NewPoller(st, goalSvc, runSvc)
	h := NewWebhookHandler("github", st, p, func(context.Context) (string, error) { return "s3cret", nil })

	payload := []byte(`{"action":"opened","issue":{"number":9,"title":"webhook 建的任务","body":"b"},"repository":{"full_name":"x/y"}}`)
	sig := "sha256=" + hmacSHA256Hex("s3cret", payload)

	if err := h.Handle(ctx, payload, sig, ""); err != nil {
		t.Fatalf("valid delivery must succeed: %v", err)
	}
	goals, _ := goalSvc.List(ctx)
	if len(goals) != 1 || goals[0].SourceRef != "github:x/y#9" {
		t.Fatalf("webhook must create the issue goal: %+v", goals)
	}
	// Duplicate delivery (webhook + poll racing) → no second goal.
	if err := h.Handle(ctx, payload, sig, ""); err != nil {
		t.Fatal(err)
	}
	goals, _ = goalSvc.List(ctx)
	if len(goals) != 1 {
		t.Fatalf("duplicate delivery must not double-create, got %d goals", len(goals))
	}
	// Bad signature → rejected.
	if err := h.Handle(ctx, payload, "sha256=deadbeef", ""); err == nil {
		t.Fatal("bad signature must be rejected")
	}
	// Non-opened action → no-op.
	closed := []byte(`{"action":"closed","issue":{"number":10},"repository":{"full_name":"x/y"}}`)
	if err := h.Handle(ctx, closed, "sha256="+hmacSHA256Hex("s3cret", closed), ""); err != nil {
		t.Fatal(err)
	}
	// Untracked repo → no-op.
	other := []byte(`{"action":"opened","issue":{"number":1},"repository":{"full_name":"z/w"}}`)
	if err := h.Handle(ctx, other, "sha256="+hmacSHA256Hex("s3cret", other), ""); err != nil {
		t.Fatal(err)
	}
	goals, _ = goalSvc.List(ctx)
	if len(goals) != 1 {
		t.Fatalf("untracked repo must not create, got %d goals", len(goals))
	}
}

// TestGitCodeClient: the v5 API shape — access_token query, close via
// PATCH /repos/{owner}/issues/{number} formData (repo in the form, not the
// path).
func TestGitCodeClient(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Get("access_token"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": "3", "title": "gitcode issue", "body": "b"}, // GitCode numbers are STRINGS
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := &GitCodeClient{token: "tok", http: srv.Client(), baseURL: srv.URL}

	issues, err := c.ListOpenIssues(context.Background(), "u/r")
	if err != nil || len(issues) != 1 || issues[0].Number != 3 {
		t.Fatalf("list: %v %+v", err, issues)
	}
	if err := c.CreateComment(context.Background(), "u/r", 3, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseIssue(context.Background(), "u/r", 3); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, " ")
	if !strings.Contains(joined, "GET /repos/u/r/issues?tok") {
		t.Fatalf("list path must carry access_token: %s", joined)
	}
	if !strings.Contains(joined, "POST /repos/u/r/issues/3/comments?tok") {
		t.Fatalf("comment path: %s", joined)
	}
	if !strings.Contains(joined, "PATCH /repos/u/issues/3?tok") {
		t.Fatalf("close path must be /repos/{owner}/issues/{number}: %s", joined)
	}
}

// TestGitCodeWebhookTokenMode: gitcode password mode verifies via the
// X-GitCode-Token header; signature mode via X-GitCode-Signature-256.
func TestGitCodeWebhookTokenMode(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	if !verifyWebhook("gitcode", "pass123", body, "", "pass123") {
		t.Fatal("password mode must verify the plain header")
	}
	if verifyWebhook("gitcode", "pass123", body, "", "wrong") {
		t.Fatal("wrong password must fail")
	}
	sig := "sha256=" + hmacSHA256Hex("pass123", body)
	if !verifyWebhook("gitcode", "pass123", body, sig, "") {
		t.Fatal("signature mode must verify")
	}
	// github endpoint must NOT accept the gitcode token header.
	if verifyWebhook("github", "pass123", body, "", "pass123") {
		t.Fatal("github endpoint must reject the gitcode token header")
	}
}
