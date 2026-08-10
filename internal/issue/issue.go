// Package issue is the GitHub issue adapter (M4-B): the external-entry half
// of the loop. Open issues in a domain's configured repo become goals
// (poller); the agent replies to issues through the platform (CreateComment,
// driven by agentwork-cli); a delivered goal closes its issue (closer).
//
// The adapter is a pure platform action: the agent never touches the GitHub
// token or the API — it produces structured side effects via agentwork-cli,
// the platform executes them (triangle separation).
package issue

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// Issue is one GitHub issue (API shape, minimal).
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	HTMLURL string `json:"html_url"`
	// PullRequest is non-nil for PRs — the issues API returns them too and
	// they must be skipped (a PR is not a work request).
	PullRequest any `json:"pull_request"`
}

// Comment is one issue comment (API shape, minimal).
type Comment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// Client talks to the GitHub REST API with a repo-scoped token (the domain's
// git_credentials). The HTTP client and base URL are injectable for tests.
type Client struct {
	token   string
	http    *http.Client
	baseURL string
}

func NewClient(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.github.com"}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ListOpenIssues returns the repo's open issues (PRs excluded).
func (c *Client) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	var issues []Issue
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues?state=open&per_page=100", nil, &issues)
	if err != nil {
		return nil, err
	}
	out := []Issue{}
	for _, i := range issues {
		if i.PullRequest == nil {
			out = append(out, i)
		}
	}
	return out, nil
}

// ListComments returns the issue's comments (newest last).
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var out []Comment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number), nil, &out)
	return out, err
}

// CreateComment posts a comment on the issue.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number),
		map[string]string{"body": body}, nil)
}

// CloseIssue closes the issue.
func (c *Client) CloseIssue(ctx context.Context, repo string, number int) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number),
		map[string]string{"state": "closed"}, nil)
}

// ── Poller: open issues → goals ──

// Poller turns a domain's open issues into goals (one goal per issue,
// idempotent via the unique source_ref). The poll is the trigger of the M4-B
// loop — no public webhook needed on a single-user machine.
type Poller struct {
	st      *store.Store
	goalSvc *service.GoalService
	runSvc  *service.RunService
	// newProvider is injectable for tests.
	newProvider func(provider, token string) (Provider, error)
}

func NewPoller(st *store.Store, goalSvc *service.GoalService, runSvc *service.RunService) *Poller {
	return &Poller{st: st, goalSvc: goalSvc, runSvc: runSvc, newProvider: NewProvider}
}

// Poll scans every tracking domain once. Returns how many goals were created.
func (p *Poller) Poll(ctx context.Context) (int, error) {
	rows, err := p.st.DB().QueryContext(ctx,
		`SELECT id, issue_provider, issue_repo, issue_assignee, git_credentials FROM domain
		 WHERE issue_repo != '' AND issue_assignee != '' AND git_credentials != ''`)
	if err != nil {
		return 0, err
	}
	type dom struct{ id, provider, repo, assignee, token string }
	var doms []dom
	for rows.Next() {
		var d dom
		if err := rows.Scan(&d.id, &d.provider, &d.repo, &d.assignee, &d.token); err != nil {
			rows.Close()
			return 0, err
		}
		doms = append(doms, d)
	}
	rows.Close()

	created := 0
	for _, d := range doms {
		n, err := p.pollDomain(ctx, d)
		created += n
		if err != nil {
			return created, err
		}
	}
	return created, nil
}

func (p *Poller) pollDomain(ctx context.Context, d struct{ id, provider, repo, assignee, token string }) (int, error) {
	client, err := p.newProvider(d.provider, d.token)
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", d.id, err)
	}
	issues, err := client.ListOpenIssues(ctx, d.repo)
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", d.id, err)
	}
	created := 0
	for _, iss := range issues {
		ok, err := p.CreateGoalForIssue(ctx, d.id, d.assignee, d.provider, d.repo, iss)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// CreateGoalForIssue turns one issue into a goal (idempotent via the unique
// source_ref). Shared by the poller AND the webhook handler — both triggers
// converge on the same create path, so webhook + poll racing can never
// double-create. Returns whether a goal was created.
func (p *Poller) CreateGoalForIssue(ctx context.Context, domainID, assigneeID, provider, repo string, iss Issue) (bool, error) {
	ref := SourceRef(provider, repo, iss.Number)
	var exists int
	if err := p.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE source_ref=?`, ref).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil // already tracked
	}
	// The goal's description carries the issue context; the run prompt
	// additionally injects the live comments (daemon side).
	title := iss.Title
	if title == "" {
		title = fmt.Sprintf("Issue #%d", iss.Number)
	}
	desc := fmt.Sprintf("来自 GitHub issue #%d：%s\n%s", iss.Number, iss.Title, truncate(iss.Body, 1500))
	g, err := p.goalSvc.Create(ctx, service.Goal{
		Title:         title,
		Description:   desc,
		DomainID:      domainID,
		AssigneeType:  "agent",
		AssigneeID:    assigneeID,
		Status:        "active",
		CreatedByType: "system",
		CreatedByID:   "issue-poll",
		SourceRef:     ref,
	})
	if err != nil {
		return false, fmt.Errorf("create goal for %s: %w", ref, err)
	}
	if _, err := p.runSvc.EnqueueForGoal(ctx, *g); err != nil {
		return false, fmt.Errorf("enqueue %s: %w", ref, err)
	}
	return true, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── Closer: delivered goal → close its issue ──

// Closer closes the issue behind a goal once the goal is delivered (merged
// into the domain's default branch). Subscribes to goal:delivered.
type Closer struct {
	st *store.Store
	// newProvider is injectable for tests.
	newProvider func(provider, token string) (Provider, error)
}

func NewCloser(st *store.Store) *Closer {
	return &Closer{st: st, newProvider: NewProvider}
}

// OnDelivered is the bus handler for goal:delivered.
func (c *Closer) OnDelivered(ctx context.Context, goalID string) {
	var ref, token string
	err := c.st.DB().QueryRowContext(ctx,
		`SELECT g.source_ref, d.git_credentials FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, goalID).
		Scan(&ref, &token)
	if err != nil || ref == "" {
		return // no domain or not issue-sourced
	}
	provider, repo, number, ok := ParseSourceRef(ref)
	if !ok {
		return
	}
	client, err := c.newProvider(provider, token)
	if err != nil {
		return
	}
	if err := client.CloseIssue(ctx, repo, number); err != nil {
		// Closing is best-effort: the work IS delivered; the issue closing can
		// be retried by hand. Log only — never fail the deliver.
		fmt.Printf("issue: close %s:%s#%d: %v\n", provider, repo, number, err)
		return
	}
	// Tell the human what happened, with the commit context.
	_ = client.CreateComment(ctx, repo, number,
		fmt.Sprintf("✅ 已由 agentwork 修复并合入（goal %s）。", short(goalID)))
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// verifyWebhook checks the platform's signature header:
//
//	github:  X-Hub-Signature-256: sha256=<hex HMAC-SHA256(secret, body)>
//	gitcode: X-GitCode-Signature-256: sha256=<hex>  (signature-key mode)
//	         X-GitCode-Token: <password>            (password mode)
func verifyWebhook(provider, secret string, body []byte, signature, tokenHeader string) bool {
	if provider == "gitcode" {
		if tokenHeader != "" {
			return hmac.Equal([]byte(tokenHeader), []byte(secret))
		}
		return verifySignature(secret, body, signature)
	}
	return verifySignature(secret, body, signature)
}

// ── Webhook: real-time trigger (M4-B) ──

// WebhookHandler receives a hosting platform's `issues` webhook events
// (opened/reopened) and creates goals immediately — the real-time trigger,
// complementing the poller (the safety net for missed deliveries). One
// handler per provider (github / gitcode): the signature header and payload
// shape differ per platform. Comment events are ignored — the run-start
// comment fetch already covers the dialogue channel.
type WebhookHandler struct {
	provider string
	st       *store.Store
	poller   *Poller
	// getSecret resolves the shared webhook secret ('' = webhook disabled).
	getSecret func(ctx context.Context) (string, error)
}

func NewWebhookHandler(provider string, st *store.Store, poller *Poller, getSecret func(ctx context.Context) (string, error)) *WebhookHandler {
	return &WebhookHandler{provider: provider, st: st, poller: poller, getSecret: getSecret}
}

// webhookEvent is the GitHub `issues` event payload (minimal shape).
type webhookEvent struct {
	Action     string `json:"action"`
	Issue      Issue  `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Handle processes one webhook delivery. Returns an HTTP status + a
// user-facing error (nil error = 200).
func (h *WebhookHandler) Handle(ctx context.Context, body []byte, signature, tokenHeader string) error {
	secret, err := h.getSecret(ctx)
	if err != nil {
		return fmt.Errorf("webhook secret lookup: %w", err)
	}
	if secret == "" {
		return fmt.Errorf("webhook not configured (no secret)")
	}
	// GitHub: X-Hub-Signature-256. GitCode: X-GitCode-Signature-256 (same
	// HMAC-SHA256) or X-GitCode-Token (plain password mode).
	if !verifyWebhook(h.provider, secret, body, signature, tokenHeader) {
		return fmt.Errorf("bad signature")
	}
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return fmt.Errorf("parse webhook payload: %w", err)
	}
	if ev.Action != "opened" && ev.Action != "reopened" {
		return nil // only issue creation triggers goals
	}
	if ev.Repository.FullName == "" || ev.Issue.Number == 0 {
		return nil
	}
	// Route to the domain tracking this repo (provider must match the
	// endpoint — a github delivery on the gitcode endpoint is not ours).
	var domainID, assignee string
	err = h.st.DB().QueryRowContext(ctx,
		`SELECT id, issue_assignee FROM domain WHERE issue_repo=? AND issue_provider=?`, ev.Repository.FullName, h.provider).
		Scan(&domainID, &assignee)
	if err != nil {
		return nil // repo not tracked — nothing to do (not an error)
	}
	if assignee == "" {
		return nil
	}
	_, err = h.poller.CreateGoalForIssue(ctx, domainID, assignee, h.provider, ev.Repository.FullName, ev.Issue)
	return err
}

// verifySignature checks X-Hub-Signature-256 ("sha256=<hex HMAC-SHA256>").
func verifySignature(secret string, body []byte, signature string) bool {
	got := "sha256=" + hmacSHA256Hex(secret, body)
	return hmac.Equal([]byte(got), []byte(signature))
}

func hmacSHA256Hex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
