// Package gitcodeapi is the GitCode (gitcode.com) repository-management API
// adapter: the write-side capability the issue package lacks (repo creation).
// Same base URL + access_token query auth as issue.GitCodeClient (v5 shape).
// Template apply uses it to create the project repo; the RepoProvider
// interface keeps the call sites platform-agnostic (a GitHub implementation
// slots in behind the same surface).
package gitcodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RepoProvider abstracts the repo-management surface template apply needs.
type RepoProvider interface {
	// CurrentUser returns the authenticated user's login (the default repo
	// owner when the template does not name an org).
	CurrentUser(ctx context.Context, token string) (string, error)
	// CreateRepo creates a repository and returns its clone URL
	// (https://gitcode.com/<owner>/<name>.git shape).
	CreateRepo(ctx context.Context, token string, in CreateRepoInput) (string, error)
}

// CreateRepoInput is the provider-neutral repo-creation request.
type CreateRepoInput struct {
	// Org is the target organization/owner ('' = the authenticated user).
	Org string
	// Name is the repo name (validated by the caller against the template).
	Name string
	// Description is the repo one-liner ('' allowed).
	Description string
	// Private sets repo visibility (true = private).
	Private bool
	// AutoInit creates the repo with an initial commit (README) so the
	// default branch exists — an empty repo cannot pass the platform's
	// domain git probe ("repository is empty").
	AutoInit bool
	// GitignoreTemplate names the platform-side .gitignore preset
	// ('' = none; e.g. "Go").
	GitignoreTemplate string
}

// Client speaks GitCode API v5 (api.gitcode.com/api/v5, access_token query
// auth — the same convention issue.GitCodeClient uses).
type Client struct {
	http *http.Client
}

// New returns a GitCode v5 client.
func New() *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second}}
}

// NewProvider returns the RepoProvider named by the platform config.
// Currently only gitcode; unknown names error (the caller surfaces 400).
func NewProvider(name string) (RepoProvider, error) {
	switch name {
	case "", "gitcode":
		return New(), nil
	default:
		return nil, fmt.Errorf("unknown repo provider %q (gitcode)", name)
	}
}

func (c *Client) do(ctx context.Context, method, path, token string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := "https://api.gitcode.com/api/v5" + path + sep + "access_token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
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
		return fmt.Errorf("gitcode %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// gitUser is the authenticated-user wire shape (only login is read).
type gitUser struct {
	Login string `json:"login"`
}

// CurrentUser resolves the token's owner login (GET /user).
func (c *Client) CurrentUser(ctx context.Context, token string) (string, error) {
	var u gitUser
	if err := c.do(ctx, http.MethodGet, "/user", token, nil, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", fmt.Errorf("gitcode /user: empty login")
	}
	return u.Login, nil
}

// gitRepo is the created-repo wire shape — full_name ("owner/name") and the
// clone URLs cover both response styles the API has shipped.
type gitRepo struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	SSHURL        string `json:"ssh_url"`
	CloneURL      string `json:"clone_url"`
	HTTPURL       string `json:"http_url"`
	EmptyRepo     *bool  `json:"empty_repo"`
	DefaultBranch string `json:"default_branch"`
}

// cloneURL prefers the explicit clone_url, then http_url, then synthesizes
// from full_name — the response fields have varied across API revisions.
func (r gitRepo) cloneURL() string {
	for _, v := range []string{r.CloneURL, r.HTTPURL} {
		if v != "" {
			return v
		}
	}
	if r.FullName != "" {
		return "https://gitcode.com/" + r.FullName + ".git"
	}
	return ""
}

// CreateRepo creates a repository (POST /orgs/{org}/repos, org = the token's
// user when in is empty). Returns the https clone URL. The v5 documented
// create endpoint is org-scoped; a personal repo is created through the
// owner's namespace.
func (c *Client) CreateRepo(ctx context.Context, token string, in CreateRepoInput) (string, error) {
	org := in.Org
	if org == "" {
		login, err := c.CurrentUser(ctx, token)
		if err != nil {
			return "", fmt.Errorf("resolve repo owner: %w", err)
		}
		org = login
	}
	body := map[string]any{
		"name":        in.Name,
		"description": in.Description,
		"private":     in.Private,
	}
	if in.AutoInit {
		body["auto_init"] = true
	}
	if in.GitignoreTemplate != "" {
		body["gitignore_template"] = in.GitignoreTemplate
	}
	var repo gitRepo
	if err := c.do(ctx, http.MethodPost, "/orgs/"+org+"/repos", token, body, &repo); err != nil {
		return "", err
	}
	clone := repo.cloneURL()
	if clone == "" {
		return "", fmt.Errorf("gitcode create repo: response missing clone URL (full_name=%q html_url=%q)", repo.FullName, repo.HTMLURL)
	}
	return clone, nil
}
