package daemon

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// deliver — the deterministic merge step (DESIGN.md §7). After the human
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

// deliverWaitForRuns bounds how long deliver waits for an in-flight run
// (the review-window squad review is the usual waiter) before giving up.
const deliverWaitForRuns = 5 * time.Minute

// recoverPendingDelivers re-runs the deliver step for goals whose approve
// never delivered (decision 2-9, trigger side): a crash between the approve
// and the merge/push leaves the goal in review with the latest gate_decision
// = approve and a review_request that is NOT a deliver failure ("deliver:" —
// that is a real failure awaiting the human's retry/reject, never replayed).
// The replay itself is safe: deliverGoal's merge/push idempotency skips
// already-done steps. Returns how many delivers were re-triggered.
func (d *Daemon) recoverPendingDelivers(ctx context.Context) (int, error) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT g.id FROM goal g
		 WHERE g.status='review'
		   AND g.review_request NOT LIKE 'deliver:%'
		   AND EXISTS (
		     SELECT 1 FROM gate_decision d
		     WHERE d.goal_id = g.id AND d.decision='approve'
		       AND d.decided_at = (SELECT MAX(decided_at) FROM gate_decision WHERE goal_id = g.id)
		   )`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		log.Printf("daemon: replaying pending deliver for %s", id)
		go d.deliverGoal(context.Background(), id)
	}
	return len(ids), nil
}

func (d *Daemon) deliverGoal(ctx context.Context, goalID string) {
	var domainID, defaultBranch, gitURL, gitCredentials, domainType string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT d.id, d.default_branch, d.git_url, d.git_credentials, COALESCE(d.type,'') FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, goalID).
		Scan(&domainID, &defaultBranch, &gitURL, &gitCredentials, &domainType)
	if err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: goal has no domain: "+err.Error())
		return
	}
	// A scratch domain has nothing to merge — the goal's project directory
	// (and its sg/ subdirectories) already holds the deliverable; the feed
	// report points at it. Approval closes the loop directly; no git steps,
	// no branch-wait (owner runs already ended — the goal parked review on
	// the owner's completion).
	if domainType == "scratch" {
		d.finishDeliver(ctx, goalID, true, "无仓库交付——产物在项目目录，按汇报验收", nil)
		return
	}
	// No in-flight OWNER run before the merge touches the goal branch (决策
	// 6-6: deliver waits only for the goal-branch writer — consult/review
	// runs are read-only snapshots and never block delivery). Bounded wait,
	// never race it. The approve/reject guard keeps the goal in review while
	// deliver runs, so the wait cannot be invalidated by a concurrent decision.
	waitDeadline := time.Now().Add(deliverWaitForRuns)
	for {
		var running int
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND status='running' AND role='owner'`, goalID).Scan(&running); err == nil && running == 0 {
			break
		}
		if time.Now().After(waitDeadline) {
			d.finishDeliver(ctx, goalID, false, "deliver: 等待运行中的 owner run 结束超时（5 分钟），请稍后再次批准")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	// Deliver INTO the domain's configured default branch (DESIGN.md §7).
	// Wrong config fails loudly with the branch name — the owner fixes the
	// domain, no silent fallbacks.
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	branchName := goalBranchName(goalID)

	// All git writes on the shared repo are serialized per domain (decision
	// 2-10): concurrent deliveries (or a fetch) would collide.
	unlock := d.lockDomain(domainID)
	defer unlock()

	repo := domainRepoPath(domainID)
	if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: prepare repo: "+err.Error())
		return
	}
	// Fresh origin/<default> BEFORE the branch checks (the merge base must be
	// the remote's current tip).
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: fetch: "+err.Error()+": "+string(out))
		return
	}

	// The goal branch must have commits AHEAD of the base — an empty branch
	// (equal to the base) must NOT be treated as "already merged" and
	// delivered empty.
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
		// No commits on the branch: nothing to merge or push. For a
		// zero-change run (behavior gate approved, a consult) the human's
		// approval IS the outcome — closing done is correct, not a failure.
		// (A real task that produced nothing surfaces through verification
		// and the human's own review of the evidence before approving.)
		log.Printf("daemon: deliver %s: branch has no commits — approving as-is", goalID)
		d.finishDeliver(ctx, goalID, true, "no changes to deliver — approved as-is")
		return
	}

	// Idempotency: already merged (crash between merge and push)?
	if _, err := gitRunCtx(ctx, repo, "merge-base", "--is-ancestor", branchName, "origin/"+defaultBranch); err == nil {
		// Branch already in origin/default — nothing to merge; the push is a
		// no-op and MarkDelivered closes.
		log.Printf("daemon: deliver %s: branch already merged (resume after crash?)", goalID)
		d.finishDeliver(ctx, goalID, true, "already merged; nothing to push")
		return
	}

	// Ephemeral deliver worktree (决策 6-2): a detached checkout of
	// origin/<default> used ONLY for this merge — the merge commit lives on
	// the detached HEAD and is pushed as <default>. Removed afterwards; a
	// crashed deliver leaves the dir behind for the startup sweep.
	wt := deliverWorktreePath(goalID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "--detach", wt, "origin/"+defaultBranch).CombinedOutput(); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: worktree add: "+err.Error()+": "+string(out))
		return
	}
	defer func() {
		if out, err := exec.CommandContext(context.Background(), "git", "-C", repo, "worktree", "remove", "--force", wt).CombinedOutput(); err != nil {
			log.Printf("daemon: deliver worktree remove %s: %v %s", goalID, err, out)
		}
	}()
	// Post-merge guards measure the merge's own diff (default-branch tip before
	// the merge .. merge commit).
	mergeBaseSHA := strings.TrimSpace(mustGitRun(ctx, wt, "rev-parse", "HEAD"))

	// The goal branch's ORIGINAL commits (base..branch) are the human-readable
	// fix evidence — the close comment must say WHAT was done, not just that
	// it was merged (the merge commit itself is the platform's noise). FULL
	// hashes so the closer can build clickable commit links. Carried
	// STRUCTURED (not parsed from the note text) — the goal layer passes them
	// through to the delivered event verbatim.
	// base came from gitRun (newline included) — it must be trimmed before
	// it becomes a rev argument, or git fails to parse "sha\n..branch" and
	// the fix evidence silently comes back empty (the close comment lost its
	// links; regression found on the live GitCode issue #3).
	fixLog, _ := gitRun(ctx, wt, "log", "--format=%H %s", strings.TrimSpace(base)+".."+branchName)
	var fixCommits []string
	for _, l := range strings.Split(fixLog, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			fixCommits = append(fixCommits, l)
		}
	}

	// Merge the goal branch (--no-ff: a merge commit makes the delivered
	// change an explicit, revertible unit).
	mergeOut, err := gitRunCtx(ctx, wt, "merge", "--no-ff", branchName, "-m", "Merge "+branchName+" (agentwork deliver)")
	if err != nil {
		// Conflict: abort and report — the ephemeral worktree is removed by
		// the defer; the goal branch stays intact for a reject-and-fix cycle.
		_, _ = gitRunCtx(ctx, wt, "merge", "--abort")
		d.finishDeliver(ctx, goalID, false, "deliver: merge conflict:\n"+mergeOut)
		return
	}

	// Re-verify the MERGED state (DESIGN.md §7): what enters the default
	// branch must be green, not just the branch in isolation. An UNFROZEN
	// policy runs nothing (the human checkpoint already covered it — the
	// goal layer only approves a review; unfrozen policies force that
	// checkpoint by design).
	checks, timeout, baseline, checksFrozen := d.loadDomainChecks(ctx, domainID)
	if checksFrozen {
		verifyReport, ok, _ := runVerification(ctx, wt, checks, timeout)
		if !ok {
			d.finishDeliver(ctx, goalID, false, "deliver: post-merge verification failed:\n"+verifyReport)
			return
		}
		if guardReport, ok := checkGuards(ctx, wt, mergeBaseSHA, checks, baseline); !ok {
			d.finishDeliver(ctx, goalID, false, "deliver: post-merge guards failed:\n"+guardReport)
			return
		}
	}

	// The merge commit lives on the detached HEAD — push it as the default
	// branch (the ephemeral worktree has no local default branch).
	if _, err := gitRunCtx(ctx, wt, "push", "origin", "HEAD:"+defaultBranch); err != nil {
		d.finishDeliver(ctx, goalID, false, "deliver: push: "+err.Error())
		return
	}

	// The delivered note carries the merge info; the fix commits travel
	// STRUCTURED to the delivered event (the close comment links them).
	note := "merged " + branchName + " → " + defaultBranch
	d.finishDeliver(ctx, goalID, true, note, fixCommits)
}

// finishDeliver closes the deliver step via the goal layer's MarkDelivered
// (the goal layer is the only authority over goal.status).
func (d *Daemon) finishDeliver(ctx context.Context, goalID string, success bool, note string, commits ...[]string) {
	log.Printf("daemon: deliver %s: %s", goalID, note)
	var fixCommits []string
	if len(commits) > 0 {
		fixCommits = commits[0]
	}
	if _, err := d.goalSvc.MarkDelivered(ctx, goalID, success, note, fixCommits); err != nil {
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
