package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/eushing/agentwork/internal/gitutil"
	"github.com/eushing/agentwork/internal/link"
)

// gitwork — the machine's git execution (CLI 分支 Phase 3): the bare repo
// per domain, the goal/sub-goal branch, the per-run worktree, the commit,
// and the branch push to the remote as agentwork/<branch> (中转 — the
// transfer point that makes the branch durable and visible to the
// platform's deliver step).

// runGit runs a git command in dir and returns trimmed combined output.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// machineRunWorkdir is where a run's worktree lives on the machine
// (~/.agentwork/runs/<runID> — one run owns its directory; crash leftovers
// are reclaimed by the same path on re-dispatch).
func machineRunWorkdir(runID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "runs", runID)
}

// ensureBareRepo clones the domain repo ONCE per machine (bare, mirrors
// remote branches into refs/remotes/origin/*) and fetches fresh state.
func ensureBareRepo(ctx context.Context, p link.RunDispatchParams) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	repo := filepath.Join(home, ".agentwork", "repos", p.DomainID)
	url := gitutil.CloneURL(p.GitURL, p.GitCredentials)
	// Existence alone is not "cloned": a half-written dir from a crashed
	// clone would pass os.Stat and then fail every fetch. A git repo has a
	// HEAD — without it, wipe and re-clone.
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil {
		_ = os.RemoveAll(repo)
		if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
			return "", err
		}
		if out, err := runGit(ctx, "", "clone", "--bare", url, repo); err != nil {
			return "", fmt.Errorf("clone: %v: %s", err, sanitizeGitOut(out, url))
		}
		// Point the fetch refspec at refs/remotes/origin/* (a bare clone
		// would otherwise mirror remote branches into local refs/heads/).
		if out, err := runGit(ctx, repo, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return "", fmt.Errorf("config fetch: %v: %s", err, out)
		}
	}
	if out, err := runGit(ctx, repo, "fetch", "origin"); err != nil {
		return "", fmt.Errorf("fetch: %v: %s", err, sanitizeGitOut(out, url))
	}
	return repo, nil
}

// sanitizeGitOut strips the embedded-token URL from git error output (the
// same guarantee the daemon's sanitizeURL provides).
func sanitizeGitOut(s, url string) string {
	return strings.ReplaceAll(s, url, gitutil.SanitizeURL(url))
}

// ensureGitWorkdir prepares the run's git worktree: the goal branch
// (feat-<goalID>, created from origin/<default>) or the sub-goal branch
// (sg-<subGoalID>, branched from the goal branch's HEAD — falling back to
// origin/<default> before the goal branch exists). Returns the workdir,
// the bare repo path, the branch name, and a cleanup func.
func ensureGitWorkdir(ctx context.Context, p link.RunDispatchParams) (workdir, repo, branch string, cleanup func(), err error) {
	repo, err = ensureBareRepo(ctx, p)
	if err != nil {
		return "", "", "", nil, err
	}
	def := p.DefaultBranch
	if def == "" {
		def = "main"
	}
	// The daemon's branch naming truncates ids to 8 chars (goalBranchName)
	// — the deliver/gate layers adopt under THAT name; a full-id name here
	// made the transfer branch invisible to them (live: "goal branch
	// feat-3d63f2f1 missing" while agentwork/feat-<full-id> sat on origin).
	g := p.GoalID
	if len(g) > 8 {
		g = g[:8]
	}
	branch = "feat-" + g
	base := "origin/" + def
	if p.SubGoalID != "" {
		// The daemon's sub-goal branch naming (feat-<goalID8>-sg-<subGoalID8>).
		sg := p.SubGoalID
		if len(sg) > 8 {
			sg = sg[:8]
		}
		branch = "feat-" + g + "-sg-" + sg
		goalBranch := "refs/heads/feat-" + p.GoalID
		if _, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", goalBranch); err != nil {
			base = "origin/" + def
		} else {
			base = goalBranch
		}
	}
	if _, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		if out, err := runGit(ctx, repo, "branch", branch, base); err != nil {
			return "", "", "", nil, fmt.Errorf("branch %s: %v: %s", branch, err, out)
		}
	}
	wt := machineRunWorkdir(p.RunID)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		if out, err := runGit(ctx, repo, "worktree", "add", "--force", wt, branch); err != nil {
			return "", "", "", nil, fmt.Errorf("worktree add: %v: %s", err, out)
		}
	}
	cleanup = func() {
		_, _ = runGit(context.Background(), repo, "worktree", "remove", "--force", wt)
	}
	return wt, repo, branch, cleanup, nil
}

// commitAndPush commits the run's changes onto the branch (when there are
// any) and pushes it to the remote as agentwork/<branch> — the 中转 push.
// A no-change run pushes nothing.
func commitAndPush(ctx context.Context, p link.RunDispatchParams, wt, repo, branch, runID, pushedProfile string) error {
	// The platform-written AGENTS.md (config.push) is NOT the agent's work:
	// when unchanged, it stays out of the commit (git add -A minus the one
	// path). An agent-modified profile IS its own file and gets committed.
	if pushedProfile != "" {
		if b, err := os.ReadFile(filepath.Join(wt, "AGENTS.md")); err == nil && string(b) == pushedProfile {
			if out, err := runGit(ctx, wt, "add", "-A", "--", ":(exclude)AGENTS.md"); err != nil {
				return fmt.Errorf("git add: %v: %s", err, out)
			}
			return commitAndPushStaged(ctx, p, wt, repo, branch, runID)
		}
	}
	if out, err := runGit(ctx, wt, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %v: %s", err, out)
	}
	return commitAndPushStaged(ctx, p, wt, repo, branch, runID)
}

// commitAndPushStaged commits/pushes whatever is staged (shared by the
// normal and AGENTS.md-excluded paths).
func commitAndPushStaged(ctx context.Context, p link.RunDispatchParams, wt, repo, branch, runID string) error {
	if _, err := runGit(ctx, wt, "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing staged — no commit, no push
	}
	args := []string{"commit", "-m", "agentwork run " + runID}
	if name, email := splitIdentity(p.GitIdentity); name != "" {
		args = append([]string{"-c", "user.name=" + name, "-c", "user.email=" + email}, args...)
	}
	if out, err := runGit(ctx, wt, args...); err != nil {
		return fmt.Errorf("commit: %v: %s", err, out)
	}
	if out, err := runGit(ctx, wt, "push", "origin", branch+":refs/heads/agentwork/"+branch); err != nil {
		return fmt.Errorf("push %s: %v: %s", branch, err, out)
	}
	return nil
}

// splitIdentity parses the domain's git_identity ("name <email>") for the
// commit author config.
func splitIdentity(identity string) (name, email string) {
	if i := strings.Index(identity, "<"); i >= 0 {
		email = strings.TrimSuffix(identity[i+1:], ">")
		name = strings.TrimSpace(identity[:i])
	}
	if strings.TrimSpace(name) == "" {
		name = "agentwork[bot]"
	}
	if strings.TrimSpace(email) == "" {
		email = "bot@local"
	}
	return strings.TrimSpace(name), strings.TrimSpace(email)
}
