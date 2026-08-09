package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/eushing/agentwork/internal/events"
)

// deliver — the deterministic merge step (DESIGN.v2.md §7). After the human
// approves the checkpoint, the platform merges the goal branch into the
// domain's default branch, re-verifies the merged state, pushes, and closes
// via MarkDelivered. The merge is a PLATFORM action, never an agent run —
// the worker must not verify its own work (invariant 9).
//
// Idempotency (decision 2-9): if the goal branch is already an ancestor of
// origin/default (crash mid-deliver), the merge is skipped and only the push
// is (re-)attempted; git push is naturally idempotent.

// onGoalApproved reacts to the human's approve: the deliver step runs
// asynchronously (it performs git operations that can take minutes).
func (d *Daemon) onGoalApproved(ctx context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	go d.deliverGoal(context.Background(), goalID)
}

func (d *Daemon) deliverGoal(ctx context.Context, goalID string) {
	var domainID, defaultBranch, gitURL string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT d.id, d.default_branch, d.git_url FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, goalID).
		Scan(&domainID, &defaultBranch, &gitURL)
	if err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: goal has no domain: "+err.Error())
		return
	}
	// Deliver INTO the domain's configured default branch (DESIGN.v2.md §7).
	// Wrong config fails loudly with the branch name — the owner fixes the
	// domain, no silent fallbacks.
	repo := domainRepoPath(domainID)
	if _, err := d.ensureGoalWorktree(ctx, domainID, goalID, gitURL, defaultBranch); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: prepare worktree: "+err.Error())
		return
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	branchName := goalBranchName(goalID)
	wt := goalWorktreePath(domainID, goalID)

	// All git writes on the shared repo are serialized per domain (decision
	// 2-10): concurrent deliveries (or a fetch) would collide.
	unlock := d.lockDomain(domainID)
	defer unlock()

	// The goal branch must have commits AHEAD of the base — an empty branch
	// (equal to the worktree base, e.g. after a shared-repo wipe orphaned the
	// branch) must NOT be treated as "already merged" and delivered empty.
	base, err := gitRun(ctx, repo, "rev-parse", "origin/"+defaultBranch)
	if err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: resolve base: "+err.Error())
		return
	}
	branchHead, err := gitRun(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branchName)
	if err != nil || strings.TrimSpace(branchHead) == "" {
		d.finishDeliver(ctx, goalID, false, "deliver: goal branch "+branchName+" missing — nothing to deliver")
		return
	}
	if strings.TrimSpace(branchHead) == strings.TrimSpace(base) {
		d.finishDeliver(ctx, goalID, false, "deliver: goal branch "+branchName+" has no commits ahead of "+defaultBranch+" — nothing to deliver (agent produced no work, or a shared-repo wipe orphaned the branch)")
		return
	}

	// Idempotency: already merged (crash between merge and push)?
	if _, err := gitRunCtx(ctx, repo, "merge-base", "--is-ancestor", branchName, "origin/"+defaultBranch); err == nil {
		// Branch already in origin/default — nothing to merge; push again is a
		// no-op and MarkDelivered closes.
		log.Printf("daemon: deliver %s: branch already merged (resume after crash?)", goalID)
		if _, perr := gitRunCtx(ctx, wt, "push", "origin", defaultBranch); perr != nil {
			d.finishDeliver(ctx, goalID, false, "deliver: push: "+perr.Error())
			return
		}
		d.finishDeliver(ctx, goalID, true, "already merged; pushed")
		return
	}

	// Switch the worktree to the default branch and sync.
	if _, err := gitRunCtx(ctx, wt, "checkout", defaultBranch); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: checkout "+defaultBranch+": "+err.Error())
		return
	}
	if _, err := gitRunCtx(ctx, wt, "pull", "--ff-only", "origin", defaultBranch); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: pull origin/"+defaultBranch+": "+err.Error())
		return
	}
	// Post-merge guards measure the merge's own diff (default-branch tip before
	// the merge .. merge commit).
	mergeBaseSHA := strings.TrimSpace(mustGitRun(ctx, wt, "rev-parse", "HEAD"))

	// Merge the goal branch (--no-ff: a merge commit makes the delivered
	// change an explicit, revertible unit).
	mergeOut, err := gitRunCtx(ctx, wt, "merge", "--no-ff", branchName, "-m", "Merge "+branchName+" (agentwork deliver)")
	if err != nil {
		// Conflict: abort, hand the worktree back to the goal branch (the
		// agent's work stays intact for a reject-and-fix cycle), and report.
		_, _ = gitRunCtx(ctx, wt, "merge", "--abort")
		_, _ = gitRunCtx(ctx, wt, "checkout", branchName)
		d.finishDeliver(ctx, goalID, false, "deliver: merge conflict:\n"+mergeOut)
		return
	}

	// Re-verify the MERGED state (DESIGN.v2.md §7): what enters the default
	// branch must be green, not just the branch in isolation.
	checks, timeout, baseline := d.loadDomainChecks(ctx, domainID)
	verifyReport, ok := runVerification(ctx, wt, checks, timeout)
	if !ok {
		// Verification red after merge: reset the default branch, hand the
		// worktree back to the goal branch, and report.
		_, _ = gitRunCtx(ctx, wt, "reset", "--hard", "origin/"+defaultBranch)
		_, _ = gitRunCtx(ctx, wt, "checkout", branchName)
		d.finishDeliver(ctx, goalID, false, "deliver: post-merge verification failed:\n"+verifyReport)
		return
	}
	if guardReport, ok := checkGuards(ctx, wt, mergeBaseSHA, checks, baseline); !ok {
		_, _ = gitRunCtx(ctx, wt, "reset", "--hard", "origin/"+defaultBranch)
		_, _ = gitRunCtx(ctx, wt, "checkout", branchName)
		d.finishDeliver(ctx, goalID, false, "deliver: post-merge guards failed:\n"+guardReport)
		return
	}

	if _, err := gitRunCtx(ctx, wt, "push", "origin", defaultBranch); err != nil {
		// Push failed (e.g. remote moved): reset local default, restore the
		// goal branch; the human can retry deliver.
		_, _ = gitRunCtx(ctx, wt, "reset", "--hard", "origin/"+defaultBranch)
		_, _ = gitRunCtx(ctx, wt, "checkout", branchName)
		d.finishDeliver(ctx, goalID, false, "deliver: push: "+err.Error())
		return
	}
	// Leave the worktree on the goal branch so a later reject/resume finds
	// the agent's state where it was.
	_, _ = gitRunCtx(ctx, wt, "checkout", branchName)

	d.finishDeliver(ctx, goalID, true, "merged "+branchName+" → "+defaultBranch)
}

// finishDeliver closes the deliver step via the goal layer's MarkDelivered
// (the goal layer is the only authority over goal.status).
func (d *Daemon) finishDeliver(ctx context.Context, goalID string, success bool, note string) {
	log.Printf("daemon: deliver %s: %s", goalID, note)
	if _, err := d.goalSvc.MarkDelivered(ctx, goalID, success, note); err != nil {
		log.Printf("daemon: MarkDelivered %s: %v", goalID, err)
	}
}

// gitRunCtx runs git in dir and reports error-ness without the output noise
// (deliver paths care about exit status; messages are returned separately).
func gitRunCtx(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRun(ctx, dir, args...)
	if err != nil {
		return strings.TrimSpace(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out), nil
}
