package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
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
		logging.Infof("daemon: replaying pending deliver for %s", id)
		go d.deliverGoal(context.Background(), id)
	}
	return len(ids), nil
}

func (d *Daemon) deliverGoal(ctx context.Context, goalID string) {
	// The goal TITLE travels with every deliver log line — ids are for the
	// system, humans read titles.
	var goalTitle string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT title FROM goal WHERE id=?`, goalID).Scan(&goalTitle)
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
	waitLogged := false
	for {
		var running int
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND status='running' AND role='owner'`, goalID).Scan(&running); err == nil && running == 0 {
			break
		}
		if !waitLogged {
			// The deliver wait is a 卡点 (up to 5 minutes): the merge is
			// held — say so, and say what holds it.
			logging.Infof("deliver: goal %q waiting for %d running owner run(s) before merge", goalTitle, running)
			waitLogged = true
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
	start := time.Now()
	logging.Infof("git: deliver %q (%s): fetch ...", goalTitle, goalID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		logging.Errorf("git: deliver %q (%s): fetch failed (%s): %s", goalTitle, goalID, time.Since(start).Round(time.Second), strings.TrimSpace(string(out)))
		d.finishDeliver(ctx, goalID, false, "deliver: fetch: "+err.Error()+": "+string(out))
		return
	}
	logging.Infof("git: deliver %q (%s): fetch done (%s)", goalTitle, goalID, time.Since(start).Round(time.Second))

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
		// Machine-executed goals (CLI 分支 Phase 3) pushed their branch to
		// the remote as agentwork/<branch> (中转) — the deliver merges THAT
		// when the local ref is absent. A local ref wins (daemon-executed
		// goals still merge their own branch).
		remoteBranch := "agentwork/" + branchName
		if _, terr := gitRun(ctx, repo, "rev-parse", "--verify", "--quiet", "origin/"+remoteBranch); terr == nil {
			if _, berr := gitRun(ctx, repo, "branch", branchName, "origin/"+remoteBranch); berr != nil {
				d.finishDeliver(ctx, goalID, false, "deliver: adopt remote branch "+remoteBranch+": "+berr.Error())
				return
			}
			logging.Infof("git: deliver %q (%s): branch %s adopted from origin/%s (machine-executed)", goalTitle, goalID, branchName, remoteBranch)
			branchHead, _ = gitRun(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branchName)
		}
	}
	if strings.TrimSpace(branchHead) == "" {
		d.finishDeliver(ctx, goalID, false, "deliver: goal branch "+branchName+" missing — nothing to deliver")
		return
	}
	if strings.TrimSpace(branchHead) == strings.TrimSpace(base) {
		// No commits on the branch: nothing to merge or push. For a
		// zero-change run (behavior gate approved, a consult) the human's
		// approval IS the outcome — closing done is correct, not a failure.
		// (A real task that produced nothing surfaces through verification
		// and the human's own review of the evidence before approving.)
		// 决策 7-4: this is still a deliver-SUCCESS outcome, so the (possibly
		// empty) feat branch is garbage — clean it before finishing, same as
		// the merged and already-merged paths. cleanupGoalBranches tolerates
		// a missing local/remote ref (the zero-commit case often has none).
		logging.Infof("daemon: deliver %q (%s): branch has no commits — approving as-is", goalTitle, goalID)
		d.cleanupGoalBranches(ctx, goalID, goalTitle, domainID, gitURL, gitCredentials)
		d.finishDeliver(ctx, goalID, true, "no changes to deliver — approved as-is")
		return
	}

	// Idempotency: already merged (crash between merge and push)?
	if _, err := gitRunCtx(ctx, repo, "merge-base", "--is-ancestor", branchName, "origin/"+defaultBranch); err == nil {
		// Branch already in origin/default — nothing to merge; the push is a
		// no-op and MarkDelivered closes. This is a deliver-SUCCESS outcome
		// (the code IS on default; the resume just detected the no-op), so
		// 决策 7-4 applies the same as the normal success path: delete the
		// now-disposable feat branch before finishing. The original code
		// returned here WITHOUT cleanup, leaking feat-<id> on both local and
		// remote for every goal resumed through this path (实测: goal
		// 25d31d96, feat branch survived on both sides after the resume).
		// Runs inside deliverGoal's held domain lock; cleanupGoalBranches
		// does not re-lock (决策 7-4 LOCK CONTRACT) — no deadlock.
		logging.Infof("daemon: deliver %q (%s): branch already merged (resume after crash?)", goalTitle, goalID)
		d.cleanupGoalBranches(ctx, goalID, goalTitle, domainID, gitURL, gitCredentials)
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
			logging.Infof("daemon: deliver worktree remove %s: %v %s", goalID, err, out)
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
	fixLog, _ := gitRun(ctx, wt, "log", "-z", "--format=%H %s%n%b", strings.TrimSpace(base)+".."+branchName)
	var fixCommits []string
	for _, l := range strings.Split(fixLog, "\x00") {
		if l = strings.TrimSpace(l); l != "" {
			fixCommits = append(fixCommits, l)
		}
	}

	// Merge the goal branch (--no-ff: a merge commit makes the delivered
	// change an explicit, revertible unit). The merge COMMIT needs a committer
	// identity — pass it via -c so a machine with no global user.name/user.email
	// (containers, fresh runners) can still create the merge commit. Same
	// resolution as commitRunChanges (verify.go); without it git aborts with
	// "Committer identity unknown", which this path used to misreport as a
	// merge conflict (deliver: merge conflict: …identity unknown…).
	mergeName, mergeEmail := "agentwork[bot]", "agentwork@local"
	if id := d.domainGitIdentity(ctx, domainID); id != "" {
		if l, r, ok := strings.Cut(id, "<"); ok && strings.Contains(r, ">") {
			mergeName = strings.TrimSpace(l)
			mergeEmail = strings.TrimSuffix(strings.TrimSpace(r), ">")
		}
	}
	start = time.Now()
	logging.Infof("git: deliver %q (%s): merge %s ...", goalTitle, goalID, branchName)
	mergeOut, err := gitRunCtx(ctx, wt, "-c", "user.name="+mergeName, "-c", "user.email="+mergeEmail, "merge", "--no-ff", branchName, "-m", "Merge "+branchName+" (agentwork deliver)")
	if err != nil {
		// Conflict or identity/config error: abort and report — the ephemeral
		// worktree is removed by the defer; the goal branch stays intact for a
		// reject-and-fix cycle. The message preserves git's own output so the
		// human can tell a real conflict from a config/identity failure (the
		// label says "merge conflict" but the body is git's verbatim stderr).
		_, _ = gitRunCtx(ctx, wt, "-c", "user.name="+mergeName, "-c", "user.email="+mergeEmail, "merge", "--abort")
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
	start = time.Now()
	logging.Infof("git: deliver %q (%s): push HEAD:%s ...", goalTitle, goalID, defaultBranch)
	if _, err := gitRunCtx(ctx, wt, "push", "origin", "HEAD:"+defaultBranch); err != nil {
		logging.Errorf("git: deliver %q (%s): push failed (%s): %v", goalTitle, goalID, time.Since(start).Round(time.Second), err)
		d.finishDeliver(ctx, goalID, false, "deliver: push: "+err.Error())
		return
	}
	logging.Infof("git: deliver %q (%s): push done (%s)", goalTitle, goalID, time.Since(start).Round(time.Second))

	// Cleanup (决策 7-4): the goal's feat branch is disposable once merged —
	// delete both the remote 中转 ref (agentwork/<branch>, machine-executed
	// goals) and the local feat-<id> ref. Best-effort + fault-tolerant inside
	// cleanupGoalBranches. The domain git creds (loaded at the top of
	// deliverGoal) are passed so the remote delete can authenticate, though
	// the bare repo's origin config already carries them.
	d.cleanupGoalBranches(ctx, goalID, goalTitle, domainID, gitURL, gitCredentials)

	// The delivered note carries the merge info; the fix commits travel
	// STRUCTURED to the delivered event (the close comment links them).
	note := "merged " + branchName + " → " + defaultBranch
	d.finishDeliver(ctx, goalID, true, note, fixCommits)
}

// finishDeliver closes the deliver step via the goal layer's MarkDelivered
// (the goal layer is the only authority over goal.status).
func (d *Daemon) finishDeliver(ctx context.Context, goalID string, success bool, note string, commits ...[]string) {
	logging.Infof("daemon: deliver %s: %s", goalID, note)
	var fixCommits []string
	if len(commits) > 0 {
		fixCommits = commits[0]
	}
	if _, err := d.goalSvc.MarkDelivered(ctx, goalID, success, note, fixCommits); err != nil {
		logging.Infof("daemon: MarkDelivered %s: %v", goalID, err)
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

// cleanupGoalBranches removes a goal's feat branch at every terminal path
// EXCEPT deliver failure (决策 7-4). The branch is goalID-derived
// (goalBranchName) and shared by every run of the goal, so it must survive a
// failed deliver — reject→active→new run continues from the branch HEAD
// (§7:294, 决策 7-2); deleting it would discard the agent's committed work.
// It is garbage on three other terminal paths:
//   - deliver SUCCESS (merged into default, branch disposable)
//   - goal CANCEL (cancelled is terminal — no cancelled→active transition)
//   - goal DELETE (the row is gone)
//
// Both refs are best-effort + fault-tolerant: a missing ref or a failed
// delete logs and moves on (the same posture as the inline delete it
// replaces). gitCredentials is needed because the remote delete pushes, and
// the bare repo's origin config already carries the credentialed URL from
// ensureSharedRepo — but passing it keeps the helper self-contained when the
// repo was set up by an earlier process.
//
// LOCK CONTRACT (决策 7-4 补充, 2026-08): this function does NOT acquire the
// per-domain git lock. The callers split into two lock states: the THREE
// deliver-success call sites inside deliverGoal (already-merged resume,
// no-changes-as-is, and the normal merged-then-pushed path) all run INSIDE
// deliverGoal's already-held lockDomain section, while the cancel and delete
// paths run in their own goroutines WITHOUT it. lockDomain uses a
// non-reentrant sync.Mutex, so re-locking from any deliver path
// self-deadlocks (the live goal 25d31d96 stalled exactly here on the normal
// success path: push succeeded, then the goroutine blocked forever waiting
// for a lock it already held, finishDeliver never ran, goal stuck in review
// with code already on main). The lock is the CALLER's responsibility: the
// deliver paths hold it (no extra acquire), cancel/delete acquire it before
// calling.
func (d *Daemon) cleanupGoalBranches(ctx context.Context, goalID, goalTitle, domainID, gitURL, gitCredentials string) {
	branchName := goalBranchName(goalID)
	repo := domainRepoPath(domainID)
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil {
		// No bare repo on disk (scratch domain, or the domain was never
		// used) — nothing to clean. Not an error.
		return
	}
	// No lockDomain here — see the LOCK CONTRACT above. The caller serializes
	// against concurrent fetch/deliver (决策 2-10); the two ref deletes below
	// are immediate and need no extra protection.

	// Remote 中转 ref: agentwork/feat-<id> (machine-executed goals push here,
	// 决策 6-25). Deleted by pushing an empty ref. Missing is the common case
	// for daemon-executed goals (they never publish a transfer ref) — git
	// returns an error, logged and ignored.
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "push", "origin", ":refs/heads/agentwork/"+branchName).CombinedOutput(); err != nil {
		logging.Infof("git: cleanup %q (%s): remote agentwork/%s: %v %s", goalTitle, goalID, branchName, err, strings.TrimSpace(string(out)))
	} else {
		logging.Infof("git: cleanup %q (%s): deleted remote agentwork/%s", goalTitle, goalID, branchName)
	}
	// Local feat-<id> ref. -D forces the delete (the branch may have unmerged
	// commits relative to default — that is expected: a cancelled goal's WIP
	// is being discarded by the terminal transition, not preserved). Missing
	// is normal for machine-executed goals (no local branch, only the remote
	// transfer ref was adopted at deliver time).
	if out, err := gitRun(ctx, repo, "branch", "-D", branchName); err != nil {
		logging.Infof("git: cleanup %q (%s): local %s: %v %s", goalTitle, goalID, branchName, err, strings.TrimSpace(out))
	} else {
		logging.Infof("git: cleanup %q (%s): deleted local %s", goalTitle, goalID, branchName)
	}
}
