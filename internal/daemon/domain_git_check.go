package daemon

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// DomainGitTestResult is the outcome of a domain git connectivity check
// (POST /domains/test): the owner validates the repo URL, the default
// branch name, and the platform token BEFORE the first run — a
// misconfigured domain used to surface as a failed first run (clone/fetch
// error) instead.
type DomainGitTestResult struct {
	OK             bool     `json:"ok"`
	BranchExists   bool     `json:"branch_exists"`
	ResolvedBranch string   `json:"resolved_branch,omitempty"` // the branch checked (configured, or the remote's HEAD default)
	Refs           []string `json:"refs,omitempty"`
	Error          string   `json:"error,omitempty"`
	LatencyMs      int64    `json:"latency_ms"`
}

// domainGitTestTimeout bounds the ls-remote — a dead host must not block the
// HTTP request.
const domainGitTestTimeout = 30 * time.Second

// TestDomainGit runs `git ls-remote` against the configured repo URL with
// the platform token embedded (the same embedding the clone uses —
// gitCloneURL) and checks the default branch exists. Read-only: ls-remote
// verifies reachability + read permission + the branch; push permission
// cannot be probed without actually pushing (the form hint documents the
// required scopes instead). The token never leaks into the returned error
// (git errors echo the embedded-token URL; sanitizeURL strips it).
func (d *Daemon) TestDomainGit(ctx context.Context, gitURL, defaultBranch, credentials string) *DomainGitTestResult {
	start := time.Now()
	res := &DomainGitTestResult{}
	defer func() { res.LatencyMs = time.Since(start).Milliseconds() }()

	if gitURL == "" {
		res.Error = "git_url is empty"
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, domainGitTestTimeout)
	defer cancel()

	url := gitCloneURL(gitURL, credentials)
	out, err := exec.CommandContext(ctx, "git", "ls-remote", "--heads", url).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		res.Error = sanitizeURL(msg)
		return res
	}
	res.OK = true
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref := fields[len(fields)-1]
		name, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok {
			continue
		}
		res.Refs = append(res.Refs, name)
	}
	// Which branch to check? The configured one — or, left blank (the create
	// form has no branch field), the remote's actual HEAD default.
	res.ResolvedBranch = defaultBranch
	if res.ResolvedBranch == "" {
		res.ResolvedBranch = d.resolveRemoteDefault(ctx, url, res.Refs)
	}
	for _, name := range res.Refs {
		if name == res.ResolvedBranch {
			res.BranchExists = true
			break
		}
	}
	return res
}

// resolveRemoteDefault finds the remote's real default branch via
// `ls-remote --symref HEAD` (the ref HEAD points at). Fallback: main, then
// master, then the first ref — the test reports what it checked either way.
func (d *Daemon) resolveRemoteDefault(ctx context.Context, url string, refs []string) string {
	if out, err := exec.CommandContext(ctx, "git", "ls-remote", "--symref", url, "HEAD").CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
				if name, ok := strings.CutPrefix(fields[1], "refs/heads/"); ok {
					return name
				}
			}
		}
	}
	for _, name := range []string{"main", "master"} {
		for _, r := range refs {
			if r == name {
				return name
			}
		}
	}
	if len(refs) > 0 {
		return refs[0]
	}
	return ""
}
