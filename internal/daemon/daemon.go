// Package daemon dispatches queued runs to agent runtimes. MVP uses the
// per-run subprocess model: each run opens a fresh transport connection via
// runtime.Open, hands it to the protocol Backend for one Prompt, and tears
// it down when the turn ends. There is no long-lived per-agent server.
//
// Concurrency is per-agent: each agent has a worker goroutine with a
// semaphore sized to agent.max_concurrent, so one agent's runs run in parallel
// up to its limit while different agents are independent.
//
// State authority is NOT here: when a run reaches a terminal status the daemon
// calls RunService.Finish, which stamps the run row then hands the outcome to
// GoalService.ReconcileOnRunEnd — the sole place that advances goal.status.
// See DESIGN.zh.md §7.
package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/runtime"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"github.com/google/uuid"
)

// dispatchTickInterval is how often the daemon claims queued runs. Claims are
// per-agent (only within the set of agents with free worker slots), so this
// bounds perceived latency without hot-looping.
const dispatchTickInterval = 500 * time.Millisecond

// scheduleTickInterval is how often the daemon scans schedule for due firings.
const scheduleTickInterval = 5 * time.Second

// worktreeCleanupInterval is how often the daemon sweeps expired goal
// worktrees (M1: every 6h).
const worktreeCleanupInterval = 6 * time.Hour

// digestTickInterval is how often the daemon checks whether the daily digest
// is due (M3: once a minute; the digest fires at most once per day).
const digestTickInterval = time.Minute

// digestDefaultTime is the daily digest time (HH:MM, local) when the owner
// has not configured notify.digest_time (DESIGN.v2.md §11 M3).
const digestDefaultTime = "09:00"

// worktreeRetentionDays is how long a terminal goal's worktree is kept after
// its last run (DESIGN.v2.md §13 — M1 value: 7 days; kept for review/debug).
const worktreeRetentionDays = 7

// workerQueueDepth bounds how many queued runs one agent's worker holds before
// back-pressuring the dispatcher.
const workerQueueDepth = 64

// defaultListenAddr is used when no addr is configured.
const defaultListenAddr = ":7373"

// idleWindow is the no-activity budget after which the idle watchdog cancels
// a hung turn. An agent that emits nothing for this long is presumed stuck.
const idleWindow = 2 * time.Minute

// idleToolWindow extends the budget while a tool is in flight (a long-running
// tool is legitimately silent between tool_use and tool_result).
const idleToolWindow = 10 * time.Minute

// maxAttempts bounds per-run retries; mirrored from service. A run that fails
// this many times leaves the goal failed for human inspection.
const maxAttempts = 3

// Daemon owns per-agent workers and the run dispatch loop.
type Daemon struct {
	st        *store.Store
	bus       *events.Bus
	addr      string
	protoReg  *proto.Registry
	goalSvc   *service.GoalService
	runSvc    *service.RunService
	squadSvc  *service.SquadService
	schedSvc  *service.ScheduleService
	im        *notify.Connector // M3: daily digest + intake replies (the notifier
	// is born when the long connection connects; fetch it live)
	qs        notify.QueryStore // M3: digest aggregation (may be nil)

	mu          sync.Mutex
	workers     map[string]*agentWorker // agentID → per-agent scheduler
	domainLocks map[string]*domainLock  // per-domain git lock (fetch + deliver)
	msgBuffers  map[string]*msgBuffer   // runID → aggregated text row (persistEvent)
	stopped     bool
	ctx         context.Context
}

// agentWorker schedules one agent's runs with a concurrency semaphore.
type agentWorker struct {
	agentID    string
	sem        chan struct{}    // capacity = max_concurrent
	queue      chan *service.ClaimedRow
	ctx        context.Context
	cancel     context.CancelFunc
	daemonCtx  context.Context
	run        func(context.Context, *service.ClaimedRow)
	maxConc    int
}

// New wires the daemon. im + qs are the M3 IM surfaces: the connector is the
// owner of the notifier (born when the long connection connects), qs feeds
// the daily digest and intake queries. Both may be nil (notify not wired).
func New(st *store.Store, bus *events.Bus, addr string, protoReg *proto.Registry, goalSvc *service.GoalService, runSvc *service.RunService, squadSvc *service.SquadService, schedSvc *service.ScheduleService, im *notify.Connector, qs notify.QueryStore) *Daemon {
	d := &Daemon{
		st: st, bus: bus, addr: addr,
		protoReg: protoReg, goalSvc: goalSvc, runSvc: runSvc,
		squadSvc: squadSvc, schedSvc: schedSvc,
		im:         im,
		qs:         qs,
		workers:    make(map[string]*agentWorker),
		msgBuffers: make(map[string]*msgBuffer),
	}
	bus.Subscribe("agent:created", d.onAgentCreated)
	bus.Subscribe("agent:deleted", d.onAgentDeleted)
	bus.Subscribe("goal:approved", d.onGoalApproved)
	return d
}

// Run starts the dispatch loop. Blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	d.recoverWorkers(ctx)
	if n, err := d.runSvc.RecoverStuckRunning(ctx); err != nil {
		log.Printf("daemon: recover stuck running: %v", err)
	} else if n > 0 {
		log.Printf("daemon: recovered %d stuck running run(s)", n)
	}
	dispatchTick := time.NewTicker(dispatchTickInterval)
	scheduleTick := time.NewTicker(scheduleTickInterval)
	cleanupTick := time.NewTicker(worktreeCleanupInterval)
	digestTick := time.NewTicker(digestTickInterval)
	defer dispatchTick.Stop()
	defer scheduleTick.Stop()
	defer cleanupTick.Stop()
	defer digestTick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("daemon: shutting down")
			d.stopAll()
			return ctx.Err()
		case <-dispatchTick.C:
			d.dispatchOnce(ctx)
		case <-scheduleTick.C:
			d.dispatchSchedules(ctx)
		case <-cleanupTick.C:
			d.cleanupWorktrees(ctx)
		case <-digestTick.C:
			d.dispatchDigest(ctx)
		}
	}
}

// ── daily digest (M3-3) ──

// dispatchDigest fires the daily summary card once per day, at the
// configured digest time (app_settings notify.digest_time, default 09:00).
// The already-sent marker (notify.digest_last_sent, date) makes the fire
// idempotent across daemon restarts.
func (d *Daemon) dispatchDigest(ctx context.Context) {
	notifier := d.imNotifier()
	if notifier == nil || d.qs == nil {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	var last string
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='notify.digest_last_sent'`).Scan(&last)
	if last == today {
		return
	}
	hhmm := digestDefaultTime
	// The digest time lives in the platform.m3 settings blob (M3 settings
	// page: 设置 → 平台设置).
	var blob string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='platform.m3'`).Scan(&blob); err == nil && blob != "" {
		var st struct {
			DigestTime string `json:"digest_time"`
		}
		if json.Unmarshal([]byte(blob), &st) == nil && st.DigestTime != "" {
			hhmm = st.DigestTime
		}
	}
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		log.Printf("daemon: digest time %q: %v", hhmm, err)
		return
	}
	digestAt := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if now.Before(digestAt) {
		return // not due yet today
	}
	// Window: yesterday 00:00 → now (the digest is a morning summary).
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	card, err := notify.BuildDigestCard(ctx, d.qs, dayStart.Add(-24*time.Hour), now)
	if err != nil {
		log.Printf("daemon: digest build: %v", err)
		return
	}
	if err := notifier.SendCard(card); err != nil {
		log.Printf("daemon: digest send: %v", err)
		return
	}
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES ('notify.digest_last_sent',?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		today, nowStr()); err != nil {
		log.Printf("daemon: digest marker: %v", err)
	}
	log.Printf("daemon: daily digest sent (%s)", today)
}

// imNotifier returns the live milestone pusher (nil before the long
// connection is up — digest and intake replies then no-op).
func (d *Daemon) imNotifier() *notify.Notifier {
	if d.im == nil {
		return nil
	}
	return d.im.Notifier()
}

// recoverWorkers rebuilds per-agent workers for every agent in the DB —
// otherwise a daemon restart has no workers for pre-existing agents.
func (d *Daemon) recoverWorkers(ctx context.Context) {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id, max_concurrent FROM agent`)
	if err != nil {
		log.Printf("daemon: recover workers: %v", err)
		return
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var id string
		var maxConcurrent int
		if err := rows.Scan(&id, &maxConcurrent); err != nil {
			continue
		}
		d.ensureWorker(id, maxConcurrent)
		n++
	}
	if n > 0 {
		log.Printf("daemon: recovered %d agent worker(s)", n)
	}
}

// ── agent worker lifecycle ──

func (d *Daemon) onAgentCreated(ctx context.Context, e events.Event) {
	a, ok := e.Payload.(service.Agent)
	if !ok {
		return
	}
	d.ensureWorker(a.ID, a.MaxConcurrent)
	log.Printf("daemon: worker ready for agent %s", a.ID)
}

func (d *Daemon) onAgentDeleted(ctx context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]string)
	if !ok {
		return
	}
	id := m["id"]
	d.mu.Lock()
	w, ok := d.workers[id]
	if ok {
		delete(d.workers, id)
	}
	d.mu.Unlock()
	if w != nil {
		w.cancel() // stop the drain; in-flight runs finish on daemonCtx
	}
	log.Printf("daemon: worker removed for agent %s", id)
}

func (d *Daemon) ensureWorker(agentID string, maxConcurrent int) *agentWorker {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if w, ok := d.workers[agentID]; ok {
		return w
	}
	w := &agentWorker{
		agentID:   agentID,
		sem:       make(chan struct{}, maxConcurrent),
		queue:     make(chan *service.ClaimedRow, workerQueueDepth),
		daemonCtx: d.ctx,
		maxConc:   maxConcurrent,
		run:       d.runTask,
	}
	w.ctx, w.cancel = context.WithCancel(d.ctx)
	d.workers[agentID] = w
	go w.loop()
	return w
}

func (w *agentWorker) loop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case q, ok := <-w.queue:
			if !ok {
				return
			}
			w.sem <- struct{}{}
			go func(q *service.ClaimedRow) {
				defer func() { <-w.sem }()
				defer func() {
					if r := recover(); r != nil {
						log.Printf("daemon: panic in runTask for run %s: %v", q.RunID, r)
					}
				}()
				w.run(w.daemonCtx, q)
			}(q)
		}
	}
}

func (d *Daemon) stopAll() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
}

// ── run dispatch ──

// dispatchOnce claims queued runs only for agents whose worker has free
// concurrency slots, then routes each to its agent's worker. This is the fix
// for the old global head-of-line blocking: a saturated agent can no longer
// stall other agents' dispatch (DESIGN.zh.md §7).
func (d *Daemon) dispatchOnce(ctx context.Context) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	// ready = agents with at least one free slot right now.
	var ready []string
	type wc struct{ id string; free, queued int }
	var dump []wc
	for id, w := range d.workers {
		free := cap(w.sem) - len(w.sem)
		queued := len(w.queue)
		if free > 0 && queued < workerQueueDepth {
			ready = append(ready, id)
		}
		dump = append(dump, wc{id, free, queued})
	}
	d.mu.Unlock()
	_ = dump

	if len(ready) == 0 {
		return
	}
	// Claim as many runs as we have free slots in total, capped per tick.
	totalFree := 0
	for _, id := range ready {
		d.mu.Lock()
		w := d.workers[id]
		d.mu.Unlock()
		if w != nil {
			totalFree += cap(w.sem) - len(w.sem)
		}
	}
	if totalFree == 0 {
		return
	}

	claimed := 0
	for claimed < totalFree {
		q, err := d.runSvc.Claim(ctx, ready)
		if err != nil {
			log.Printf("daemon: claim: %v", err)
			return
		}
		if q == nil {
			return // nothing left to claim
		}
		d.mu.Lock()
		w, ok := d.workers[q.AgentID]
		d.mu.Unlock()
		if !ok {
			// Agent lost its worker mid-dispatch; requeue by finishing as
			// failed+retry is wrong here — just leave queued for next tick.
			log.Printf("daemon: no worker for agent %s (run %s)", q.AgentID, q.RunID)
			return
		}
		select {
		case w.queue <- q:
			claimed++
		default:
			// Worker queue full; the claim is already 'running'. Bail and let
			// the next tick re-evaluate. (Rare; bounded by queueDepth.)
			log.Printf("daemon: worker queue full for agent %s", q.AgentID)
			return
		}
	}
}

// ── worktree model (DESIGN.v2.md §6) ──
//
// Layout:
//
//	{workspaceRoot}/domains/{domainID}/repo/     shared repo (cloned once)
//	{workspaceRoot}/domains/{domainID}/wt-{goalID}/  per-goal worktree
//
// The domain owns the shared repo; each goal gets its own worktree + branch,
// so multiple goals develop the same repo in parallel without interference.
// Worktrees are allocated lazily (decision 2-18): the first run of a goal
// creates it; later runs of the same goal reuse it — which is what makes
// checkpoint resume (A5) work: the file-state is physically still there.
// git operations on the shared repo (fetch, and every deliver) are
// serialized per domain (decision 2-10): concurrent fetches would collide on
// index.lock.

// workspaceRoot is where domain repos and goal worktrees live.
func workspaceRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentwork-workspaces")
	}
	return filepath.Join(home, ".agentwork", "workspaces")
}

func domainRepoPath(domainID string) string {
	return filepath.Join(workspaceRoot(), "domains", domainID, "repo")
}

func goalWorktreePath(domainID, goalID string) string {
	return filepath.Join(workspaceRoot(), "domains", domainID, "wt-"+goalID)
}

// goalBranchName is the branch a goal's worktree works on.
func goalBranchName(goalID string) string {
	if len(goalID) > 8 {
		goalID = goalID[:8]
	}
	return "feat-" + goalID
}

// domainLocks serializes git write operations per domain (fetch + deliver —
// decision 2-10). fetch and deliver on different domains run concurrently.
type domainLock struct {
	mu  sync.Mutex
	ref int
}

func (d *Daemon) lockDomain(domainID string) func() {
	d.mu.Lock()
	if d.domainLocks == nil {
		d.domainLocks = make(map[string]*domainLock)
	}
	l := d.domainLocks[domainID]
	if l == nil {
		l = &domainLock{}
		d.domainLocks[domainID] = l
	}
	l.ref++
	d.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		d.mu.Lock()
		l.ref--
		if l.ref == 0 {
			delete(d.domainLocks, domainID)
		}
		d.mu.Unlock()
	}
}

// ensureSharedRepo clones the domain repo ONCE as a BARE repository. Bare is
// the correct shape for the worktree model: a regular clone's main worktree
// holds one branch checked out, and git refuses to check that branch out in
// any other worktree ("already checked out") — which breaks deliver when the
// domain's default branch is the same one the main worktree sits on. A bare
// repo has no main worktree, so every branch is free to be checked out in any
// goal worktree. (git worktree add works against bare repos, git 2.5+.)
// Credentials: git_credentials is not yet wired into the clone (M0
// single-user; the caller's global git config / URL-embedded credentials
// apply).
func (d *Daemon) ensureSharedRepo(ctx context.Context, domainID, gitURL string) error {
	repo := domainRepoPath(domainID)
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", gitURL, repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone --bare %s: %w: %s", gitURL, err, string(out))
	}
	// A bare clone mirrors remote branches into LOCAL refs/heads/ (its
	// remote.origin.fetch is "+refs/heads/*:refs/heads/*") and creates NO
	// refs/remotes/origin/* — so resolveDefaultBranch, worktree add
	// (origin/<branch>), and deliver would all fail to find the remote's
	// branches. Point the fetch refspec at refs/remotes/origin/* instead so
	// the rest of the code sees the usual remote-tracking namespace.
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		return fmt.Errorf("git config remote.origin.fetch: %w: %s", err, string(out))
	}
	return nil
}

// ensureGoalWorktree lazily allocates (decision 2-18) and syncs the goal's
// worktree: fetches the shared repo (under the domain lock) and, if the
// worktree does not exist yet, creates it on a fresh branch from the domain's
// default branch. Returns the worktree path.
func (d *Daemon) ensureGoalWorktree(ctx context.Context, domainID, goalID, gitURL, defaultBranch string) (string, error) {
	wt := goalWorktreePath(domainID, goalID)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return wt, nil
	}
	unlock := d.lockDomain(domainID)
	defer unlock()

	if err := d.ensureSharedRepo(ctx, domainID, gitURL); err != nil {
		return "", err
	}
	repo := domainRepoPath(domainID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch: %w: %s", err, string(out))
	}
	// Create the branch from the domain's configured default branch
	// (DESIGN.v2.md §6: the domain owns default_branch). If origin/
	// {defaultBranch} does not exist, the error names it — the domain config
	// is wrong and the owner fixes it. No silent fallbacks.
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "-b", goalBranchName(goalID), wt, "origin/"+defaultBranch)
	if out, err := cmd.CombinedOutput(); err != nil {
		// The branch may already exist from an earlier run (resume path).
		if exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", wt, goalBranchName(goalID)).Run() != nil {
			return "", fmt.Errorf("git worktree add: %w: %s", err, string(out))
		}
	}
	return wt, nil
}

// gitRun runs a git command in dir and returns its combined output.
func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustGitRun is gitRun for callers that need the (trimmed) output and can
// tolerate a failure returning "" (baseline lookups — an empty baseline is
// handled by the callers).
func mustGitRun(ctx context.Context, dir string, args ...string) string {
	out, _ := gitRun(ctx, dir, args...)
	return strings.TrimSpace(out)
}

// insertFiredGoal inserts the schedule-fired goal row inside the caller's
// transaction. Extracted as its own function because its column/value
// mapping regressed twice (excess VALUES were silently accepted and wrote
// 'active' into assignee_id with status left empty) — the regression test
// calls THIS, not a copy.
func insertFiredGoal(ctx context.Context, tx *sql.Tx, goalID, title, desc, domainID, assigneeType, assigneeID, scheduleID, ts string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,domain_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,'active','','system',?,?)`,
		goalID, title, desc, domainID, assigneeType, assigneeID, scheduleID, ts)
	return err
}

// ── worktree lifecycle (M1) ──

// cleanupWorktrees removes worktrees of goals that reached a terminal state
// more than worktreeRetentionDays ago. The goal's branch stays in the shared
// repo (history is preserved); only the checkout is reclaimed.
func (d *Daemon) cleanupWorktrees(ctx context.Context) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT g.id, g.domain_id, MAX(r.finished_at)
		 FROM goal g JOIN run r ON r.goal_id = g.id
		 WHERE g.status IN ('done','failed','cancelled')
		 GROUP BY g.id, g.domain_id`)
	if err != nil {
		log.Printf("daemon: cleanup worktrees: query: %v", err)
		return
	}
	type row struct{ goalID, domainID, finished string }
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.goalID, &r.domainID, &r.finished); err != nil {
			continue
		}
		found = append(found, r)
	}
	rows.Close()

	cutoff := time.Now().Add(-worktreeRetentionDays * 24 * time.Hour)
	for _, r := range found {
		if r.domainID == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, r.finished)
		if err != nil || t.After(cutoff) {
			continue
		}
		wt := goalWorktreePath(r.domainID, r.goalID)
		if _, err := os.Stat(wt); err != nil {
			continue
		}
		unlock := d.lockDomain(r.domainID)
		if out, err := gitRun(ctx, domainRepoPath(r.domainID), "worktree", "remove", "--force", wt); err != nil {
			log.Printf("daemon: cleanup worktree %s: %v %s", r.goalID, err, out)
		} else {
			log.Printf("daemon: removed worktree for terminal goal %s (retention expired)", r.goalID)
		}
		unlock()
	}
}

// ── run execution ──

// runTask opens a transport, hands it to the protocol backend for one Prompt,
// drains events into chat_message + WS, and finishes the run (which triggers
// goal-layer reconciliation). Processor runs (no goal — run_kind=processor,
// e.g. the acceptance-policy compiler) take the runProcessorTask path.
func (d *Daemon) runTask(ctx context.Context, q *service.ClaimedRow) {
	if q.GoalID == "" {
		d.runProcessorTask(ctx, q)
		return
	}
	var title, desc, handoff, domainID, gitURL, defaultBranch, systemPrompt, transport, provider, execPath, argsJSON, endpoint, rtEnvJSON string
	var maxConcurrent, maxRunDuration int
	var isLeaderRun bool
	var squadID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT g.title, g.description, g.handoff_note, d.id, d.git_url, d.default_branch, a.system_prompt,
		        r.transport, r.provider, r.executable, r.args, r.endpoint, r.env, a.max_concurrent, d.max_run_duration,
		        r2.is_leader_run, r2.squad_id
		 FROM run r2
		 JOIN goal g ON g.id = r2.goal_id
		 LEFT JOIN domain d ON d.id = g.domain_id
		 JOIN agent a ON a.id = r2.agent_id
		 JOIN runtime r ON r.id = a.runtime_id
		 WHERE r2.id = ?`, q.RunID).
		Scan(&title, &desc, &handoff, &domainID, &gitURL, &defaultBranch, &systemPrompt, &transport, &provider, &execPath, &argsJSON, &endpoint, &rtEnvJSON, &maxConcurrent, &maxRunDuration, &isLeaderRun, &squadID)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("load config: %v", err))
		return
	}

	d.ensureWorker(q.AgentID, maxConcurrent)

	// Working directory (DESIGN.v2.md §6): the run works in the goal's
	// worktree — lazily allocated on first run, reused on later runs of the
	// same goal (this is what makes checkpoint resume work: the file state is
	// physically still there). Every agent-executed run belongs to a domain
	// (Create and Assign enforce it), so there is no scratch fallback.
	if domainID == "" {
		d.failRun(ctx, q, "run's goal has no domain — cannot allocate a worktree")
		return
	}
	runRowWorkdir, err := d.ensureGoalWorktree(ctx, domainID, q.GoalID, gitURL, defaultBranch)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("prepare workdir: %v", err))
		return
	}
	// The run's diff baseline: guards and evidence measure this run's changes
	// as baseSHA..HEAD (the agent may commit itself, and the daemon commits
	// leftover work at run end — both land in HEAD).
	baseSHA := strings.TrimSpace(mustGitRun(ctx, runRowWorkdir, "rev-parse", "HEAD"))

	// Inject the agent's identity + team roster / squad briefing into the
	// workdir so the agent subprocess discovers who it is and who it can hand
	// off to (AGENTWORK.md).
	roster := d.buildAgentGuide(ctx, q.AgentID)
	briefing := ""
	if isLeaderRun && squadID != "" {
		owns := d.goalOwnsSquadStatus(ctx, q.GoalID, squadID)
		if b, err := d.squadSvc.BuildLeaderBriefing(ctx, squadID, owns); err == nil {
			briefing = b
		}
	}
	if err := d.injectAgentProfile(runRowWorkdir, systemPrompt, roster, briefing); err != nil {
		log.Printf("daemon: inject agent profile for run %s: %v", q.RunID, err)
	}

	// Parse runtime args + env.
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		d.failRun(ctx, q, fmt.Sprintf("parse args: %v", err))
		return
	}
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, q.AgentID)

	// Build the task environment: inherit parent, layer agent env, inject
	// agentwork-cli context so the agent can call back into the server.
	taskEnv := os.Environ()
	for k, v := range agentEnv {
		taskEnv = append(taskEnv, k+"="+v)
	}
	selfBin, _ := os.Executable()
	binDir := filepath.Dir(selfBin)
	cliPath := filepath.Join(binDir, "agentwork-cli")
	if _, err := os.Stat(cliPath); err != nil {
		log.Printf("daemon: agentwork-cli not found at %s; agent tool calls will fail", cliPath)
	}
	addr := d.addr
	if addr == "" {
		addr = defaultListenAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("parse listen addr %q: %v", addr, err))
		return
	}
	serverURL := "http://" + net.JoinHostPort("127.0.0.1", port)
	taskEnv = append(taskEnv,
		"AGENTWORK_SERVER_URL="+serverURL,
		"AGENTWORK_GOAL_ID="+q.GoalID,   // product-plane id (CLI comments/handoff)
		"AGENTWORK_RUN_ID="+q.RunID,     // execution-plane id
		"AGENTWORK_AGENT_ID="+q.AgentID,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	// Open the transport (stdio/ws/tcp); the backend speaks the protocol.
	spec := runtime.Spec{
		Transport:  transport,
		Executable: execPath,
		Args:       args,
		Endpoint:   endpoint,
		Env:        rtEnv,
	}
	conn, err := runtime.Open(ctx, spec, taskEnv)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("open transport: %v", err))
		return
	}

	// Prompt the run: title + description + handoff/wakeup note.
	prompt := buildPrompt(title, desc, handoff)

	backend, err := d.protoReg.Get(provider)
	if err != nil {
		conn.Close()
		d.failRun(ctx, q, fmt.Sprintf("provider %q: %v", provider, err))
		return
	}
	// maxRunDuration (DESIGN.v2.md §4, decision 2-6): the run's total time
	// budget, independent of activity — a run stuck in a tool loop must not
	// burn forever. The idle watchdog (silence) and this (total time) are
	// complementary cancellers of the same promptCtx. On timeout the backend
	// reports StatusCancelled; the run is cancelled WITHOUT consuming attempt
	// credit, the goal stays active, and the human is notified (M1 notify).
	if maxRunDuration <= 0 {
		maxRunDuration = 7200 // default 2h
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var inFlightTools atomic.Int32
	promptCtx, promptCancel := context.WithTimeout(ctx, time.Duration(maxRunDuration)*time.Second)
	defer promptCancel()
	go d.runIdleWatchdog(ctx, &lastActivity, &inFlightTools, promptCancel, q.RunID)

	run, err := backend.Execute(promptCtx, proto.ExecuteSpec{Conn: conn, Cwd: runRowWorkdir, Prompt: prompt})
	if err != nil {
		conn.Close()
		d.failRun(ctx, q, fmt.Sprintf("execute: %v", err))
		return
	}

	// Record workdir once the session is live (best-effort; the backend may not
	// return a session id until Result).
	_ = d.runSvc.MarkSession(ctx, q.RunID, "", runRowWorkdir)

	for ev := range run.Events {
		lastActivity.Store(time.Now().UnixNano())
		d.persistEvent(ctx, q.RunID, ev)
		d.trackToolInflight(&inFlightTools, ev)
		d.bus.Publish(ctx, events.Event{Topic: "run:event", Payload: map[string]any{
			"run_id": q.RunID, "event": ev,
		}})
	}

	result, ok := <-run.Result
	conn.Close()
	if !ok {
		result = proto.Result{Status: proto.StatusFailed, Output: "backend closed result channel"}
	}
	// The event stream is done — flush the aggregated text buffer so the last
	// message of the turn is persisted (persistEvent aggregates per-role).
	d.flushRunMessages(ctx, q.RunID)
	switch result.Status {
	case proto.StatusCompleted:
		// The handoff/wakeup note is consumed by the goal layer (ReconcileOnRunEnd
		// clears it only after confirming this run owns the goal and the goal
		// promotes to done). The daemon must NOT clear it here: on a handoff this
		// run no longer owns the goal, and clearing would wipe the new owner's
		// note (see P2 in the bug review).
		_ = d.runSvc.MarkSession(ctx, q.RunID, result.SessionID, runRowWorkdir)

		if domainID != "" {
			// Make the agent's work durable on the goal branch (the agent is
			// guided to commit; the daemon guarantees it — deliver merges the
			// branch, and uncommitted work would deliver nothing).
			if err := commitRunChanges(ctx, runRowWorkdir, d.domainGitIdentity(ctx, domainID)); err != nil {
				d.finishRun(ctx, q, "failed", "commit run changes: "+err.Error())
				return
			}
			// Machine verification (DESIGN.v2.md §4/§5, invariant 14): the
			// domain's verify commands run BEFORE the run is finished. A red
			// verify ends the run failed → retry chain. The goal layer only
			// ever sees 'completed' runs that passed.
			checks, timeout, baseline := d.loadDomainChecks(ctx, domainID)
			verifyReport, ok := runVerification(ctx, runRowWorkdir, checks, timeout)
			if !ok {
				d.finishRun(ctx, q, "failed", "verification failed:\n"+verifyReport)
				return
			}
			// Structural guards on the diff (DESIGN.v2.md §5.1), measured as
			// baseSHA..HEAD — the run's own changes. git status would be empty
			// here: the daemon just committed the agent's work (and the agent
			// may have committed itself), so the worktree is clean.
			guardReport, ok := checkGuards(ctx, runRowWorkdir, baseSHA, checks, baseline)
			if !ok {
				d.finishRun(ctx, q, "failed", "guards failed:\n"+guardReport)
				return
			}
			// Gate evaluation (M2 rule engine): merge always fires; diff_*
			// fire on the run's changed paths. The fired gates are recorded on
			// the run row — the goal layer reads them in the reconcile
			// transaction (the daemon computes, the goal layer judges).
			gatesHit := evalGates(ctx, runRowWorkdir, baseSHA, checks)
			if len(gatesHit) > 0 {
				gatesJSON, _ := json.Marshal(gatesHit)
				if _, err := d.st.DB().ExecContext(ctx, `UPDATE run SET gates_hit=? WHERE id=?`, string(gatesJSON), q.RunID); err != nil {
					log.Printf("daemon: record gates_hit for run %s: %v", q.RunID, err)
				}
			}
			// Evidence bundle for the approval card (decision 2-3).
			ev := buildEvidence(ctx, runRowWorkdir, baseSHA, result.Output, verifyReport, guardReport)
			if _, err := d.st.DB().ExecContext(ctx, `UPDATE run SET evidence=? WHERE id=?`, ev, q.RunID); err != nil {
				log.Printf("daemon: store evidence for run %s: %v", q.RunID, err)
			}
		}
		d.finishRunOK(ctx, q, result.Output)
	case proto.StatusCancelled:
		// decision 2-6 + the "stuck active with no run" hole: a cancelled run
		// does NOT fail the goal, but it also must not silently leave the goal
		// orphaned — retry once (attempt-bounded, so repeated stalls surface
		// instead of looping). If retries are exhausted, the goal stays active
		// and the notification below tells the owner to take over.
		if q.Attempt < maxAttempts {
			var isLeader int
			var squadID string
			_ = d.st.DB().QueryRowContext(ctx, `SELECT is_leader_run, squad_id FROM run WHERE id=?`, q.RunID).Scan(&isLeader, &squadID)
			if err := d.runSvc.EnqueueExisting(ctx, q.GoalID, q.AgentID, q.Attempt+1, isLeader != 0, squadID); err != nil {
				log.Printf("daemon: requeue cancelled run %s: %v", q.RunID, err)
			}
		}
		d.finishRun(ctx, q, "cancelled", "idle watchdog: "+result.Output)
		// Surface the stall so the notify layer can tell the owner a task
		// stalled (cancelled runs leave the goal active with no pending run —
		// the human decides, per decision 2-6).
		d.bus.Publish(ctx, events.Event{Topic: "run:cancelled", Payload: map[string]any{
			"run_id": q.RunID, "goal_id": q.GoalID, "reason": "idle watchdog: " + result.Output,
		}})
	case proto.StatusFailed, proto.StatusAborted:
		d.finishRun(ctx, q, "failed", result.Output)
	}
}

// runProcessorTask executes a platform-internal processor run: opens the
// agent's transport, sends the run's fixed prompt, drains events, and then
// collects the FILE-based result — the compiled checks.json + strength.txt —
// from the run workdir and stores it on the associated domain in an UNFROZEN
// state (checks_compiled_at stays ''), publishing domain:compiled so the
// frontend can show the owner confirmation card. Structured output is read
// from files, never parsed from agent stdout (DESIGN.v2.md §5.3, §9.3).
func (d *Daemon) runProcessorTask(ctx context.Context, q *service.ClaimedRow) {
	var prompt, runType, domainID, agentID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT r2.prompt, r2.run_type, r2.domain_id, r2.agent_id FROM run r2 WHERE r2.id=?`, q.RunID).
		Scan(&prompt, &runType, &domainID, &agentID)
	if err != nil {
		log.Printf("daemon: processor run %s: load config: %v", q.RunID, err)
		d.failProcessorRun(ctx, q, "load config: "+err.Error())
		return
	}
	// The intake pipeline (M3-4) has no domain — its coalesce key is the
	// inbound message id (see IntakeService.Enqueue).
	if runType == "intake" {
		d.runIntakeTask(ctx, q, prompt, agentID)
		return
	}
	if prompt == "" || domainID == "" {
		d.failProcessorRun(ctx, q, "processor run missing prompt or domain_id")
		return
	}

	var systemPrompt, transport, provider, execPath, argsJSON, endpoint, rtEnvJSON string
	var maxConcurrent int
	err = d.st.DB().QueryRowContext(ctx,
		`SELECT a.system_prompt, r.transport, r.provider, r.executable, r.args, r.endpoint, r.env, a.max_concurrent
		 FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`, agentID).
		Scan(&systemPrompt, &transport, &provider, &execPath, &argsJSON, &endpoint, &rtEnvJSON, &maxConcurrent)
	if err != nil {
		d.failProcessorRun(ctx, q, "load agent runtime: "+err.Error())
		return
	}
	d.ensureWorker(agentID, maxConcurrent)

	// Scratch workdir for the compile run (no repo — the processor works from
	// the prompt alone and writes its result files here).
	runRowWorkdir := filepath.Join(workspaceRoot(), "proc", q.RunID)
	if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
		d.failProcessorRun(ctx, q, "mkdir workdir: "+err.Error())
		return
	}

	var args []string
	_ = json.Unmarshal([]byte(argsJSON), &args)
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, agentID)
	taskEnv := os.Environ()
	for k, v := range agentEnv {
		taskEnv = append(taskEnv, k+"="+v)
	}
	// The processor agent is not a goal worker: no AGENTWORK_GOAL_ID/RUN_ID
	// injection (it should not call back into the platform), just the server
	// URL and its own identity for orientation.
	addr := d.addr
	if addr == "" {
		addr = defaultListenAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		d.failProcessorRun(ctx, q, "parse listen addr: "+err.Error())
		return
	}
	taskEnv = append(taskEnv, "AGENTWORK_SERVER_URL=http://"+net.JoinHostPort("127.0.0.1", port))

	conn, err := runtime.Open(ctx, runtime.Spec{
		Transport: transport, Executable: execPath, Args: args, Endpoint: endpoint, Env: rtEnv,
	}, taskEnv)
	if err != nil {
		d.failProcessorRun(ctx, q, "open transport: "+err.Error())
		return
	}
	defer conn.Close()

	backend, err := d.protoReg.Get(provider)
	if err != nil {
		d.failProcessorRun(ctx, q, "provider "+provider+": "+err.Error())
		return
	}
	run, err := backend.Execute(ctx, proto.ExecuteSpec{Conn: conn, Cwd: runRowWorkdir, Prompt: prompt})
	if err != nil {
		d.failProcessorRun(ctx, q, "execute: "+err.Error())
		return
	}
	for ev := range run.Events {
		d.persistEvent(ctx, q.RunID, ev)
		d.bus.Publish(ctx, events.Event{Topic: "run:event", Payload: map[string]any{
			"run_id": q.RunID, "event": ev,
		}})
	}
	result, ok := <-run.Result
	if !ok {
		result = proto.Result{Status: proto.StatusFailed, Output: "backend closed result channel"}
	}
	d.flushRunMessages(ctx, q.RunID)
	switch result.Status {
	case proto.StatusCompleted:
		// Read the compiled policy from the run workdir (file = structured
		// side effect), validate, and store on the domain unfrozen.
		checksPath := filepath.Join(runRowWorkdir, "checks.json")
		raw, err := os.ReadFile(checksPath)
		if err != nil {
			d.failProcessorRun(ctx, q, "read checks.json: "+err.Error())
			return
		}
		var checks service.Checks
		if err := json.Unmarshal(raw, &checks); err != nil {
			d.failProcessorRun(ctx, q, "parse checks.json: "+err.Error())
			return
		}
		if len(checks.Verify) == 0 && len(checks.Guards) == 0 {
			d.failProcessorRun(ctx, q, "checks.json: no verify or guards produced")
			return
		}
		strength := "medium"
		if sraw, err := os.ReadFile(filepath.Join(runRowWorkdir, "strength.txt")); err == nil {
			if v := strings.TrimSpace(string(sraw)); v == "strong" || v == "medium" || v == "weak" {
				strength = v
			}
		}
		checksJSON, _ := json.Marshal(checks)
		// The compiled policy ALWAYS lands (a fresh compile cycle replaces
		// the previous one wholesale — DESIGN.v2.md §5.3), and resets the
		// freeze stamp: the domain returns to the pending-confirmation state
		// so the owner's confirmation card reappears with the NEW product.
		// (Regression: the old UPDATE was gated on checks_compiled_at='',
		// which made a recompile AFTER freezing a silent no-op — the new
		// product was discarded and runs kept verifying with the old policy.)
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE domain SET checks=?, verification_strength=?, checks_compiled_at='' WHERE id=?`,
			string(checksJSON), strength, domainID); err != nil {
			d.failProcessorRun(ctx, q, "store compiled checks: "+err.Error())
			return
		}
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE run SET status='completed', result_summary=?, finished_at=? WHERE id=?`,
			result.Output, nowStr(), q.RunID); err != nil {
			log.Printf("daemon: finish processor run %s: %v", q.RunID, err)
		}
		d.bus.Publish(ctx, events.Event{Topic: "domain:compiled", Payload: map[string]any{
			"domain_id": domainID, "run_id": q.RunID,
		}})
		log.Printf("daemon: domain %s acceptance policy compiled (strength=%s)", domainID, strength)
	case proto.StatusCancelled:
		d.failProcessorRun(ctx, q, "idle watchdog: "+result.Output)
	case proto.StatusFailed, proto.StatusAborted:
		d.failProcessorRun(ctx, q, result.Output)
	}
}

// failProcessorRun marks a processor run failed and notifies the frontend that
// compilation did not complete (manual checks input remains the fallback).
func (d *Daemon) failProcessorRun(ctx context.Context, q *service.ClaimedRow, summary string) {
	log.Printf("daemon: processor run %s failed: %s", q.RunID, summary)
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET status='failed', result_summary=?, finished_at=? WHERE id=?`,
		summary, nowStr(), q.RunID); err != nil {
		log.Printf("daemon: mark processor run %s failed: %v", q.RunID, err)
	}
	d.bus.Publish(ctx, events.Event{Topic: "domain:compile_failed", Payload: map[string]any{
		"run_id": q.RunID, "error": summary,
	}})
}

// nowStr is the daemon-side UTC timestamp helper (service.now is private).
func nowStr() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// trackToolInflight maintains the in-flight-tool counter the idle watchdog
// reads to decide whether to use idleWindow or the larger idleToolWindow.
func (d *Daemon) trackToolInflight(n *atomic.Int32, ev proto.Event) {
	switch ev.Type {
	case proto.EventToolUse:
		n.Add(1)
	case proto.EventToolResult:
		n.Add(-1)
		if v := n.Load(); v < 0 {
			n.Store(0)
		}
	}
}

// persistEvent stores one protocol event into chat_message (the run detail
// view's data source). Consecutive text/thought chunks from the same role
// are AGGREGATED into one row (an ACP stream emits per-token chunks — a raw
// per-chunk insert produced 20k+ rows per run and destroyed transcript
// replay quality); tool events flush the pending buffer first.
func (d *Daemon) persistEvent(ctx context.Context, runID string, ev proto.Event) {
	switch ev.Type {
	case proto.EventMessage, proto.EventThought:
		role := "assistant"
		if ev.Type == proto.EventThought {
			role = "thought"
		}
		d.mu.Lock()
		b := d.msgBuffers[runID]
		if b == nil || b.role != role {
			d.flushMsgBuffer(ctx, runID)
			b = &msgBuffer{role: role}
			d.msgBuffers[runID] = b
		}
		b.content += ev.Text
		d.mu.Unlock()
	case proto.EventToolUse, proto.EventToolResult:
		d.mu.Lock()
		d.flushMsgBuffer(ctx, runID)
		d.mu.Unlock()
		tc, _ := json.Marshal(ev)
		d.insertChatMessage(ctx, runID, "tool", "", string(tc))
	default:
		d.insertChatMessage(ctx, runID, "assistant", ev.Text, "[]")
	}
}

// msgBuffer is the pending aggregated text row for a run (see persistEvent).
type msgBuffer struct {
	role    string
	content string
}

// flushMsgBuffer writes the pending aggregated text row (if any) for a run.
// Caller holds d.mu.
func (d *Daemon) flushMsgBuffer(ctx context.Context, runID string) {
	b := d.msgBuffers[runID]
	if b == nil || b.content == "" {
		return
	}
	d.insertChatMessage(ctx, runID, b.role, b.content, "[]")
	delete(d.msgBuffers, runID)
}

// flushRunMessages flushes and forgets a run's pending buffer (run end).
func (d *Daemon) flushRunMessages(ctx context.Context, runID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushMsgBuffer(ctx, runID)
	delete(d.msgBuffers, runID)
}

func (d *Daemon) insertChatMessage(ctx context.Context, runID, role, content, toolCalls string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO chat_message (id, run_id, role, content, tool_calls, created_at) VALUES (?,?,?,?,?,?)`,
		uuid.NewString(), runID, role, content, toolCalls, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		log.Printf("daemon: persist event for run %s: %v", runID, err)
	}
}

func (d *Daemon) loadAgentEnv(ctx context.Context, agentID string) (map[string]string, error) {
	var envJSON string
	err := d.st.DB().QueryRowContext(ctx, `SELECT env FROM agent WHERE id=?`, agentID).Scan(&envJSON)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(envJSON), &m)
	return m, nil
}

// failRun records a launch-time failure (before any backend ran). Failed runs
// still flow through Finish → reconcile so the goal layer can retry/fail the
// goal authoritatively.
func (d *Daemon) failRun(ctx context.Context, q *service.ClaimedRow, summary string) {
	log.Printf("daemon: run %s failed at launch: %s", q.RunID, summary)
	d.finishRun(ctx, q, "failed", summary)
}

// finishRunOK ends a successful run.
func (d *Daemon) finishRunOK(ctx context.Context, q *service.ClaimedRow, output string) {
	d.finishRun(ctx, q, "completed", output)
}

// finishRun writes the run's terminal status + hands the outcome to the goal
// layer. The goal layer (ReconcileOnRunEnd) is the SOLE authority over
// goal.status; the daemon never writes goal.status directly.
func (d *Daemon) finishRun(ctx context.Context, q *service.ClaimedRow, status, summary string) {
	if err := d.runSvc.Finish(ctx, q.RunID, status, summary); err != nil {
		log.Printf("daemon: finish run %s: %v", q.RunID, err)
	}
	// If this run was for a sub-goal, notify the goal layer to consider waking
	// the parent. (The parent's wakeup is owned by GoalService.NotifyChildDone,
	// guarded by status='blocked' + all-children-terminal inside its tx.)
	if q.GoalID != "" {
		d.runSvc.NotifyChildDone(ctx, q.GoalID)
	}
}

// goalOwnsSquadStatus mirrors multica's ownsIssueStatus: a leader run may only
// push the goal to done when the goal is assigned to THIS squad (DESIGN.zh.md
// §5.4). A guest @mentioned squad gets the "do NOT change status" briefing.
func (d *Daemon) goalOwnsSquadStatus(ctx context.Context, goalID, squadID string) bool {
	var at, aid string
	err := d.st.DB().QueryRowContext(ctx, `SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).Scan(&at, &aid)
	if err != nil {
		return false
	}
	return at == "squad" && aid == squadID
}

// buildPrompt assembles the opening prompt for a run turn.
func buildPrompt(title, desc, handoff string) string {
	body := desc
	if strings.TrimSpace(body) == "" {
		// A sub-goal created via `goal create` may carry only a title. Without a
		// body the agent gets an empty task and may not terminate; give it an
		// explicit, completable instruction so the run reaches a terminal state.
		body = "Complete this sub-task. Do the work it implies, then finish your turn."
	}
	guide := "Read AGENTWORK.md in the working directory first — it is the coordination guide for this run (team roster, agentwork-cli reference, how to hand off / fan out / request approval)."
	if handoff != "" {
		// A handoff/wakeup note scopes THIS turn. It is placed AHEAD of the
		// original description, which is now *context* (not a fresh to-do
		// list). This prevents the sub-goal loop: a woken parent that sees the
		// child-summary note must NOT blindly re-execute the original
		// description's "create a sub-task" steps. If the note reports the work
		// already complete, the agent ends its turn rather than fanning out
		// again.
		return guide + "\n\n" +
			"Task: " + title + "\n\n" +
			"Context (what this goal is about; do NOT blindly redo these steps):\n" + body + "\n\n" +
			"Scope for THIS run (follow the note; do not redo steps it describes as done):\n> " + handoff + "\n\n" +
			"If the note reports the work is already complete, do NOT start new work — end your turn immediately.\n"
	}
	return guide + "\n\n" + fmt.Sprintf("Task: %s\n\n%s", title, body)
}

// buildAgentGuide writes the "## Team & Coordination" block that every run's
// AGENTWORK.md gets: the roster of teammates plus the full agentwork-cli
// reference the agent uses to produce structured side effects. This is the
// agent's only source of truth for how to coordinate — without it the agent
// doesn't know goal create/wait/comment exist, and it would never guess the
// mention:// URI format. See DESIGN §5 (coordination primitives).
func (d *Daemon) buildAgentGuide(ctx context.Context, selfAgentID string) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id, name, description FROM agent ORDER BY name`)
	if err != nil {
		log.Printf("daemon: build team roster: %v", err)
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("## Team & Coordination\n\n")
	b.WriteString("You coordinate with other agents by calling `agentwork-cli`, which is on")
	b.WriteString(" your PATH. The server URL, goal id, run id, and your agent id are in your")
	b.WriteString(" environment (AGENTWORK_SERVER_URL / AGENTWORK_GOAL_ID / AGENTWORK_RUN_ID")
	b.WriteString(" / AGENTWORK_AGENT_ID). The CLI calls back over the server; do NOT edit files")
	b.WriteString(" to communicate intent — structured side effects via the CLI are the only way.\n\n")
	b.WriteString("This goal's id is the value of AGENTWORK_GOAL_ID.\n\n")

	b.WriteString("### Sub-goals (fan out then wait)\n")
	b.WriteString("- Create a sub-goal that another agent works on:\n")
	b.WriteString("  agentwork-cli goal create --title \"<title>\" [--description \"<what to do>\"] --assignee <other-agent-id>\n")
	b.WriteString("  (parent defaults to the current goal.)\n")
	b.WriteString("- Once you have created all the sub-goals you want to fan out, wait for them:\n")
	b.WriteString("  agentwork-cli goal wait\n")
	b.WriteString("  Then END YOUR TURN. When every sub-goal reaches a terminal state, the system")
	b.WriteString(" re-runs THIS goal with a wakeup note summarizing what each sub-goal produced.\n\n")

	b.WriteString("### Hand off the current goal\n")
	b.WriteString("- agentwork-cli goal assign <to-agent-id> [--note \"scoping instruction\"]\n")
	b.WriteString("  Hands the goal's ownership to that agent. Your current run keeps running to")
	b.WriteString(" completion, but its result no longer affects the goal — the new owner's run\n")
	b.WriteString("  drives it forward. Include a --note that scopes the work for the new owner.\n\n")

	b.WriteString("### Pull in another agent via @mention\n")
	b.WriteString("- Post a comment on the current goal mentioning an agent to trigger a run on it:\n")
	b.WriteString("  agentwork-cli goal comment --text \"[@Name](mention://agent/<AGENT-UUID>) <what you want>\"\n")
	b.WriteString("- The mention MUST be a structured Markdown link with a mention:// URI; bare")
	b.WriteString(" `@handle` prose does NOT trigger anything. Resolve the UUID first with")
	b.WriteString(" `agentwork-cli agent list` (JSON output). mention://agent/<uuid> triggers a")
	b.WriteString(" new run on that agent for THIS goal; mention://squad/<uuid> routes to the")
	b.WriteString(" squad's leader. @all (mention://all/all) suppresses auto-trigger.\n\n")

	b.WriteString("### Inspect\n")
	b.WriteString("- agentwork-cli goal list      # all goals (JSON)\n")
	b.WriteString("- agentwork-cli agent list    # all agents — use this to get the UUIDs for assign/mention\n")
	b.WriteString("- agentwork-cli squad list    # all squads\n\n")

	b.WriteString("### Team roster\n")
	b.WriteString("If a task falls outside your role, delegate it — create a sub-goal for a")
	b.WriteString(" teammate whose role best matches, or hand off the goal entirely.\n\n")
	var n int
	for rows.Next() {
		var id, name, desc string
		if err := rows.Scan(&id, &name, &desc); err != nil {
			continue
		}
		if id == selfAgentID {
			continue
		}
		fmt.Fprintf(&b, "- %s (id: %s)", name, id)
		if desc != "" {
			b.WriteString(" — ")
			b.WriteString(desc)
		}
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		b.WriteString("(you are the only agent — no teammates to delegate to)\n")
	}
	return b.String()
}

// injectAgentProfile writes the agent's system prompt, team roster, and (for
// leader runs) squad briefing into {workdir}/AGENTWORK.md so the subprocess
// discovers its identity via its native config file.
func (d *Daemon) injectAgentProfile(workdir, systemPrompt, roster, briefing string) error {
	var b strings.Builder
	if systemPrompt != "" {
		b.WriteString(systemPrompt)
		b.WriteString("\n\n")
	}
	if briefing != "" {
		b.WriteString(briefing)
		b.WriteString("\n\n")
	}
	b.WriteString(roster)
	return os.WriteFile(filepath.Join(workdir, "AGENTWORK.md"), []byte(b.String()), 0o644)
}

// runIdleWatchdog cancels a Prompt if the agent emits nothing for idleWindow
// (or idleToolWindow while a tool is in flight). It ticks at window/2.
func (d *Daemon) runIdleWatchdog(parent context.Context, lastActivity *atomic.Int64, inFlightTools *atomic.Int32, cancel context.CancelFunc, runID string) {
	interval := idleWindow / 2
	if interval <= 0 {
		interval = idleWindow
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
			threshold := idleWindow
			if inFlightTools.Load() > 0 {
				threshold = idleToolWindow
			}
			last := time.Unix(0, lastActivity.Load())
			if time.Since(last) < threshold {
				continue
			}
			log.Printf("daemon: idle watchdog firing for run %s (silent %s), force-stopping", runID, time.Since(last).Round(time.Second))
			cancel()
			return
		}
	}
}

// ── schedule dispatch ──

// dispatchSchedules fires every enabled schedule whose next_run_at is due,
// cloning a fresh goal + run. Idempotent via uq_schedule_run_planned.
func (d *Daemon) dispatchSchedules(ctx context.Context) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT id, title_template, description, assignee_type, assignee_id, domain_id, cron_expression, timezone, next_run_at
		 FROM schedule WHERE enabled=1 AND next_run_at != '' AND next_run_at <= ?`, nowStr)
	if err != nil {
		log.Printf("daemon: schedule query: %v", err)
		return
	}
	var due []scheduleDueRow
	for rows.Next() {
		var r scheduleDueRow
		if err := rows.Scan(&r.ScheduleID, &r.TitleTemplate, &r.Description, &r.AssigneeType, &r.AssigneeID, &r.DomainID, &r.CronExpression, &r.Timezone, &r.NextRunAt); err != nil {
			rows.Close()
			log.Printf("daemon: schedule scan: %v", err)
			return
		}
		due = append(due, r)
	}
	rows.Close()
	for _, r := range due {
		d.fireSchedule(ctx, r)
	}
}

type scheduleDueRow struct {
	ScheduleID, TitleTemplate, Description, AssigneeType, AssigneeID, DomainID, CronExpression, Timezone, NextRunAt string
}

func (d *Daemon) fireSchedule(ctx context.Context, r scheduleDueRow) {
	plannedAt := r.NextRunAt
	goalID := uuid.NewString()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		log.Printf("daemon: schedule %s begin tx: %v", r.ScheduleID, err)
		return
	}
	defer tx.Rollback()
	// 11 columns → exactly 11 values: id,title,description,domain_id,
	// assignee_type,assignee_id as parameters; status='active',
	// handoff_note='', created_by_type='system' literal; created_by_id=
	// schedule id, created_at=ts as parameters.
	if err := insertFiredGoal(ctx, tx, goalID, r.TitleTemplate, r.Description, r.DomainID, r.AssigneeType, r.AssigneeID, r.ScheduleID, ts); err != nil {
		log.Printf("daemon: schedule %s insert goal: %v", r.ScheduleID, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schedule_run (id,schedule_id,goal_id,planned_at,status,created_at) VALUES (?,?,?,?,'dispatched',?)`,
		uuid.NewString(), r.ScheduleID, goalID, plannedAt, ts); err != nil {
		// Unique index violation → a concurrent tick already fired this
		// planned_at. Roll back the goal insert and just advance next_run_at.
		log.Printf("daemon: schedule %s already fired at %s, skipping", r.ScheduleID, plannedAt)
		d.advanceScheduleNextRun(ctx, r, plannedAt)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("daemon: schedule %s commit: %v", r.ScheduleID, err)
		return
	}
	// Enqueue the first run for the new goal via the services (coalesce + leader
	// routing for squad assignees).
	g, err := d.goalSvc.Get(ctx, goalID)
	if err != nil {
		log.Printf("daemon: schedule %s load goal: %v", r.ScheduleID, err)
	} else if _, err := d.runSvc.EnqueueForGoal(ctx, *g); err != nil {
		log.Printf("daemon: schedule %s enqueue run: %v", r.ScheduleID, err)
	}
	d.advanceScheduleNextRun(ctx, r, plannedAt)
	d.bus.Publish(ctx, events.Event{Topic: "schedule:fired", Payload: map[string]any{
		"schedule_id": r.ScheduleID, "goal_id": goalID, "planned_at": plannedAt,
	}})
	log.Printf("daemon: schedule %s fired, created goal %s", r.ScheduleID, goalID)
}

func (d *Daemon) advanceScheduleNextRun(ctx context.Context, r scheduleDueRow, plannedAt string) {
	anchor, err := time.Parse(time.RFC3339Nano, plannedAt)
	if err != nil {
		anchor = time.Now().UTC()
	}
	next, err := service.ComputeNextRun(r.CronExpression, r.Timezone, anchor)
	if err != nil {
		log.Printf("daemon: schedule %s advance cron: %v", r.ScheduleID, err)
		return
	}
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE schedule SET next_run_at=?, last_run_at=? WHERE id=?`,
		next.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), r.ScheduleID); err != nil {
		log.Printf("daemon: schedule %s advance next_run_at: %v", r.ScheduleID, err)
	}
}