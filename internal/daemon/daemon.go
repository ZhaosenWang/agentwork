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
// See DESIGN.md
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/mcp"
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
// has not configured notify.digest_time (DESIGN.md §11 M3).
const digestDefaultTime = "09:00"

// issuePollInterval is the default issue-trigger latency (M4-B: the trigger
// is a poll — no public webhook on a single-user machine). Default 30s so an
// issue becomes a goal quickly; the owner can raise it via app_settings
// (platform.issue_poll_interval, seconds) for many tracked repos + rate
// limits. issuePollMinInterval is the tick floor (rate-limit protection).
const (
	issuePollInterval    = 30 * time.Second
	issuePollMinInterval = 15 * time.Second
)

// worktreeRetentionDays is how long a terminal goal's worktree is kept after
// its last run (DESIGN.md §13 — M1 value: 7 days; kept for review/debug).
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
	st       *store.Store
	bus      *events.Bus
	addr     string
	protoReg *proto.Registry
	goalSvc    *service.GoalService
	runSvc     *service.RunService
	commentSvc *service.CommentService
	agentSvc   *service.AgentService
	squadSvc   *service.SquadService
	schedSvc *service.ScheduleService
	im       *notify.Connector // M3: daily digest + intake replies (the notifier
	// is born when the long connection connects; fetch it live)
	qs            notify.QueryStore     // M3: digest aggregation (may be nil)
	issuePoll     *issue.Poller         // M4-B: open issues → goals
	issueCloser   *issue.Closer         // M4-B: delivered goal → close its issue
	intakeSvc     *notify.IntakeService // M4-B: multi-domain clarification draft store
	lastIssuePoll time.Time             // last poll time (the interval is configurable)

	mu          sync.Mutex
	workers     map[string]*agentWorker // agentID → per-agent scheduler
	domainLocks map[string]*domainLock  // per-domain git lock (fetch + deliver)
	msgBuffers  map[string]*msgBuffer   // runID → aggregated text row (persistEvent)
	// runCancels maps runID → the run's prompt cancel (registered by runTask,
	// used to terminate a running run when its goal changes hands — a handed
	// off agent that keeps running deadlocks the new owner's queued run behind
	// per-goal serialization; the platform cuts the old run instead of waiting
	// on the agent's good behavior). Guarded by mu.
	runCancels map[string]context.CancelFunc
	// runCancelReasons maps runID → why the run was cancelled ("idle
	// watchdog" / "handoff" / "approval"). The cancelled branch reads it ONCE
	// to stamp the real reason into result_summary — without it every cancel
	// (maxRunDuration deadline, handoff, approval) would be mislabeled "idle
	// watchdog", which both lies in the feed and poisons the convergence
	// counter (a handoff cancel must not count as a watchdog stall). Guarded
	// by mu.
	runCancelReasons map[string]string
	// mcpExecs maps runID → the run's workspace MCP executor (DESIGN.md
	// 决策 4-8: agents that don't delegate tools to client RPCs get the
	// workspace through an MCP server advertised at session/new). Guarded
	// by mu.
	mcpExecs map[string]*mcp.Executor
	stopped  bool
	ctx      context.Context
}

// agentWorker schedules one agent's runs with a concurrency semaphore.
type agentWorker struct {
	agentID   string
	sem       chan struct{} // capacity = max_concurrent
	queue     chan *service.ClaimedRow
	ctx       context.Context
	cancel    context.CancelFunc
	daemonCtx context.Context
	run       func(context.Context, *service.ClaimedRow)
	maxConc   int
}

// New wires the daemon. im + qs are the M3 IM surfaces: the connector is the
// owner of the notifier (born when the long connection connects), qs feeds
// the daily digest and intake queries. Both may be nil (notify not wired).
func New(st *store.Store, bus *events.Bus, addr string, protoReg *proto.Registry, goalSvc *service.GoalService, runSvc *service.RunService, commentSvc *service.CommentService, agentSvc *service.AgentService, squadSvc *service.SquadService, schedSvc *service.ScheduleService, im *notify.Connector, qs notify.QueryStore, intakeSvc *notify.IntakeService) *Daemon {
	d := &Daemon{
		st: st, bus: bus, addr: addr,
		protoReg: protoReg, goalSvc: goalSvc, runSvc: runSvc, commentSvc: commentSvc, agentSvc: agentSvc,
		squadSvc: squadSvc, schedSvc: schedSvc,
		im:               im,
		qs:               qs,
		intakeSvc:        intakeSvc,
		issuePoll:        issue.NewPoller(st, goalSvc, runSvc),
		issueCloser:      issue.NewCloser(st),
		workers:          make(map[string]*agentWorker),
		msgBuffers:       make(map[string]*msgBuffer),
		mcpExecs:         make(map[string]*mcp.Executor),
		runCancels:       make(map[string]context.CancelFunc),
		runCancelReasons: make(map[string]string),
	}
	bus.Subscribe("agent:created", d.onAgentCreated)
	bus.Subscribe("agent:deleted", d.onAgentDeleted)
	bus.Subscribe("goal:approved", d.onGoalApproved)
	// Handoff (human reassign or `goal assign`): the goal's new owner takes
	// over — the previous owner's running run must NOT keep going (it would
	// deadlock the new run behind per-goal serialization: an agent that
	// handed off believes its turn is over, so nothing stops its run, and the
	// new owner's run waits queued forever). The platform cuts the old run.
	bus.Subscribe("goal:assigned", d.onGoalAssigned)
	// Squad review checkpoint: a squad-owned goal parking in review triggers
	// the squad's role=reviewer members (the squad's own rule, enforced by
	// the platform — not by the leader's discretion).
	bus.Subscribe("goal:reviewing", d.onGoalReviewing)
	// Cancel now terminates the goal's running run too (决策 4-12): a
	// cancelled goal must not keep an agent burning compute on work that is
	// already decided dead. Same stop mechanism as StopRun.
	bus.Subscribe("goal:finished", d.onGoalFinished)
	// M4-B: a delivered issue-sourced goal closes its GitHub issue (the
	// work is merged — the issue is done). The fix commits (structured, from
	// the deliver) travel into the close comment so the issue records WHAT
	// was done with clickable links.
	bus.Subscribe("goal:delivered", func(_ context.Context, e events.Event) {
		m, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		goalID, _ := m["goal_id"].(string)
		note, _ := m["note"].(string)
		var commits []string
		if raw, ok := m["commits"].([]string); ok {
			commits = raw
		}
		if goalID != "" {
			d.issueCloser.OnDelivered(context.Background(), goalID, note, commits)
		}
	})
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
	// Decision 2-9, trigger side: an approve followed by a crash leaves the
	// goal in review with the approve recorded and no deliver — re-run the
	// deliver (its merge/push idempotency makes the replay safe).
	if n, err := d.recoverPendingDelivers(ctx); err != nil {
		log.Printf("daemon: recover pending delivers: %v", err)
	} else if n > 0 {
		log.Printf("daemon: re-delivering %d goal(s) whose approve never delivered", n)
	}
	dispatchTick := time.NewTicker(dispatchTickInterval)
	scheduleTick := time.NewTicker(scheduleTickInterval)
	cleanupTick := time.NewTicker(worktreeCleanupInterval)
	digestTick := time.NewTicker(digestTickInterval)
	issueTick := time.NewTicker(issuePollInterval)
	defer dispatchTick.Stop()
	defer scheduleTick.Stop()
	defer cleanupTick.Stop()
	defer digestTick.Stop()
	defer issueTick.Stop()
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
		case <-issueTick.C:
			d.dispatchIssues(ctx)
		}
	}
}

// dispatchIssues polls tracked repos for new open issues and turns them into
// goals (M4-B). The interval bounds how quickly a new issue reaches the
// queue — no public webhook needed on a single-user machine. The ticker
// fires at the minimum interval; the effective interval (default 30s,
// app_settings platform.issue_poll_interval in seconds, floor 15s) gates the
// actual poll.
func (d *Daemon) dispatchIssues(ctx context.Context) {
	interval := issuePollInterval
	var raw string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='platform.issue_poll_interval'`).Scan(&raw); err == nil && raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec >= int(issuePollMinInterval/time.Second) {
			interval = time.Duration(sec) * time.Second
		}
	}
	d.mu.Lock()
	now := time.Now()
	if now.Sub(d.lastIssuePoll) < interval {
		d.mu.Unlock()
		return
	}
	d.lastIssuePoll = now
	d.mu.Unlock()

	n, err := d.issuePoll.Poll(ctx)
	if err != nil {
		log.Printf("daemon: issue poll: %v", err)
		return
	}
	if n > 0 {
		log.Printf("daemon: issue poll created %d goal(s)", n)
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
	// Window: yesterday 00:00 → today 00:00 (the digest is a morning summary;
	// a goal finishing this morning belongs to TOMORROW's digest).
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	card, err := notify.BuildDigestCard(ctx, d.qs, dayStart.Add(-24*time.Hour), dayStart, now)
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

// Poller exposes the issue poller for the server's webhook wiring (M4-B:
// both triggers share the same create-goal path).
func (d *Daemon) Poller() *issue.Poller { return d.issuePoll }

// MCPExecutor returns the workspace MCP executor for a run (nil if the run
// is not active). The server's /mcp/{runID} route resolves through this.
func (d *Daemon) MCPExecutor(runID string) *mcp.Executor {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mcpExecs[runID]
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

// onGoalAssigned reacts to a goal changing hands (human reassign or an
// agent's `goal assign`): the goal's OLD owner's running run must be cut.
// Without this a handed-off agent keeps running — it believes its turn is
// over (the handoff was the point of its turn), so nothing stops it, and the
// new owner's run waits queued forever behind per-goal serialization: a
// deadlock. The cancel flows through the normal terminal path (promptCtx
// cancel → backend reports cancelled → reconcile discards the orphaned run;
// no attempt consumed, no auto-retry — the convergence rule only counts
// idle-watchdog cancellations). The old run's worktree leftovers stay
// attributable (a prior cancelled run), so the new owner's dirty check
// passes.
func (d *Daemon) onGoalAssigned(_ context.Context, e events.Event) {
	g, ok := e.Payload.(*service.Goal)
	if !ok {
		return
	}
	// The goal's new owner as an agent id (the only runs allowed to keep
	// running on the goal). Human owner → no agent may keep running.
	// DB work uses d.ctx, NOT the published event's ctx — the publisher is
	// often an HTTP handler whose ctx is cancelled the moment it returns
	// (see onGoalReviewing).
	ownerAgent := ""
	if g.AssigneeType == "agent" {
		ownerAgent = g.AssigneeID
	} else if g.AssigneeType == "squad" {
		_ = d.st.DB().QueryRowContext(d.ctx, `SELECT leader_id FROM squad WHERE id=?`, g.AssigneeID).Scan(&ownerAgent)
	}
	rows, err := d.st.DB().QueryContext(d.ctx,
		`SELECT id, agent_id FROM run WHERE goal_id=? AND status='running'`, g.ID)
	if err != nil {
		log.Printf("daemon: handoff cancel scan for %s: %v", g.ID, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var runID, agentID string
		if err := rows.Scan(&runID, &agentID); err != nil {
			continue
		}
		if agentID == ownerAgent {
			continue // the new owner's own run (re-assign to self) keeps going
		}
		d.mu.Lock()
		cancel, ok := d.runCancels[runID]
		if ok {
			d.runCancelReasons[runID] = "handoff"
		}
		d.mu.Unlock()
		if ok {
			log.Printf("daemon: handoff cut run %s (agent %s no longer owns goal %s)", runID, agentID, g.ID)
			cancel()
			continue
		}
		// The claim→register window: the run was claimed (status='running')
		// but runTask hasn't registered its cancel yet — the in-memory cut
		// missed it. Stamp the run terminal in the DB; runTask's post-register
		// self-check sees status != 'running' and self-cancels. The stamp is
		// the only writer besides runTask itself, so no race with finishRun.
		if _, err := d.st.DB().ExecContext(d.ctx,
			`UPDATE run SET status='cancelled', finished_at=? WHERE id=? AND status='running'`, nowStr(), runID); err != nil {
			log.Printf("daemon: handoff terminal stamp %s: %v", runID, err)
		} else {
			log.Printf("daemon: handoff stamped run %s terminal (claim→register window)", runID)
		}
	}
}

// StopRun terminates a running run on human command (决策 4-12): the run
// cancels (no attempt consumed, no auto-retry — the convergence rule only
// counts idle-watchdog stalls), the goal state is untouched, and the
// worktree keeps its state — recovery is the human's call (re-trigger /
// hand off / re-review), same as a timeout per 决策 2-6.
func (d *Daemon) StopRun(goalID, runID string) error {
	var g string
	if err := d.st.DB().QueryRowContext(d.ctx, `SELECT goal_id FROM run WHERE id=?`, runID).Scan(&g); err != nil {
		return fmt.Errorf("stop run: %v", err)
	}
	if g != goalID {
		return fmt.Errorf("run %s does not belong to goal %s", runID, goalID)
	}
	d.cancelRun(runID, "stopped")
	return nil
}

// cancelRun terminates a running run (if its cancel is registered), recording
// why. Idempotent.
func (d *Daemon) cancelRun(runID, reason string) {
	d.mu.Lock()
	cancel, ok := d.runCancels[runID]
	if ok {
		d.runCancelReasons[runID] = reason
	}
	d.mu.Unlock()
	if ok {
		log.Printf("daemon: cut run %s (%s)", runID, reason)
		cancel()
	}
}

// takeCancelReason reads and clears the run's cancellation reason — the
// cancelled branch stamps it into result_summary once, so a handoff cut is
// recorded as a handoff, not as an idle-watchdog stall.
func (d *Daemon) takeCancelReason(runID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runCancelReasons[runID]
	delete(d.runCancelReasons, runID)
	return r
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
// stall other agents' dispatch (DESIGN.md).
func (d *Daemon) dispatchOnce(ctx context.Context) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	// ready = agents with at least one free slot right now.
	var ready []string
	type wc struct {
		id           string
		free, queued int
	}
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
			// Worker queue full (rare — bounded by workerQueueDepth): the
			// claim already stamped the run 'running', so bailing would leave
			// a dead run that never reaches a worker (stuck until restart).
			// Return it to queued — the next tick re-claims it, attempt
			// untouched.
			log.Printf("daemon: worker queue full for agent %s — returning run %s to queued", q.AgentID, q.RunID)
			if _, err := d.st.DB().ExecContext(ctx,
				`UPDATE run SET status='queued', started_at='' WHERE id=?`, q.RunID); err != nil {
				log.Printf("daemon: requeue overflow run %s: %v", q.RunID, err)
			}
			return
		}
	}
}

// ── worktree model (DESIGN.md §6) ──
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
//
// Credentials (M4): the domain's git_credentials (a platform token) is
// injected into the HTTPS clone URL as the username — the machine-identity
// convention GitHub, GitLab and Gitee all accept. The credentialed URL
// persists in the bare repo's origin config, so EVERY later fetch/push
// (agent branches AND deliver's main push) inherits it — one credential
// configures the whole repo lifecycle. SSH repos keep their own auth
// (keys); git_credentials is a no-op there.
func (d *Daemon) ensureSharedRepo(ctx context.Context, domainID, gitURL, gitCredentials string) error {
	repo := domainRepoPath(domainID)
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
		return err
	}
	cloneURL := gitCloneURL(gitURL, gitCredentials)
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", cloneURL, repo)
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

// gitCloneURL injects the domain's credentials into an HTTPS clone URL. The
// username convention differs PER HOST (surfaced by the first live GitCode
// run: the agent's push with the token as username was rejected, while the
// oauth2: prefix — the GitLab PAT format GitCode speaks — worked):
//
//	github.com → token as the username (machine-identity convention)
//	gitcode.com → oauth2:TOKEN (GitLab-style PAT)
//
// Unknown hosts fall back to token-as-username. A URL that already carries
// credentials (the owner embedded them explicitly) is left untouched; SSH
// URLs are returned as-is.
func gitCloneURL(gitURL, credentials string) string {
	if credentials == "" || !strings.HasPrefix(gitURL, "https://") || strings.Contains(gitURL, "@") {
		return gitURL
	}
	cred := credentials
	if strings.Contains(gitURL, "gitcode.com") {
		cred = "oauth2:" + credentials
	}
	return "https://" + cred + "@" + strings.TrimPrefix(gitURL, "https://")
}

// ensureGoalWorktree lazily allocates (decision 2-18) and syncs the goal's
// worktree: fetches the shared repo (under the domain lock) and, if the
// worktree does not exist yet, creates it on a fresh branch from the domain's
// default branch. Returns the worktree path.
func (d *Daemon) ensureGoalWorktree(ctx context.Context, domainID, goalID, gitURL, gitCredentials, defaultBranch string) (string, error) {
	wt := goalWorktreePath(domainID, goalID)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return wt, nil
	}
	unlock := d.lockDomain(domainID)
	defer unlock()

	if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
		return "", err
	}
	repo := domainRepoPath(domainID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch: %w: %s", err, string(out))
	}
	// Create the branch from the domain's configured default branch
	// (DESIGN.md §6: the domain owns default_branch). If origin/
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
	var title, desc, handoff, domainID, gitURL, defaultBranch, systemPrompt, transport, provider, execPath, argsJSON, endpoint, rtEnvJSON, sourceRef, gitCredentials string
	var triggerAuthor, triggerCommentID string
	var maxConcurrent, maxRunDuration int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT g.title, g.description, g.handoff_note, d.id, d.git_url, d.default_branch, a.system_prompt,
		        r.transport, r.provider, r.executable, r.args, r.endpoint, r.env, a.max_concurrent, d.max_run_duration,
		        g.source_ref, d.git_credentials,
		        r2.trigger_comment_id, COALESCE(c.author_type, '')
		 FROM run r2
		 JOIN goal g ON g.id = r2.goal_id
		 LEFT JOIN domain d ON d.id = g.domain_id
		 JOIN agent a ON a.id = r2.agent_id
		 JOIN runtime r ON r.id = a.runtime_id
		 LEFT JOIN comment c ON c.id = r2.trigger_comment_id
		 WHERE r2.id = ?`, q.RunID).
		Scan(&title, &desc, &handoff, &domainID, &gitURL, &defaultBranch, &systemPrompt, &transport, &provider, &execPath, &argsJSON, &endpoint, &rtEnvJSON, &maxConcurrent, &maxRunDuration, &sourceRef, &gitCredentials, &triggerCommentID, &triggerAuthor)
	// Review run (DESIGN.md 决策 4-4): triggered by a SYSTEM comment — the
	// platform's review request ("请审查本次改动…只提意见"). Its product is
	// the opinion, not code: no commit of its worktree changes.
	reviewRun := triggerAuthor == "system"
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("load config: %v", err))
		return
	}

	d.ensureWorker(q.AgentID, maxConcurrent)

	// Working directory (DESIGN.md §6): the run works in the goal's
	// worktree — lazily allocated on first run, reused on later runs of the
	// same goal (this is what makes checkpoint resume work: the file state is
	// physically still there). Every agent-executed run belongs to a domain
	// (Create and Assign enforce it), so there is no scratch fallback.
	if domainID == "" {
		d.failRun(ctx, q, "run's goal has no domain — cannot allocate a worktree")
		return
	}
	runRowWorkdir, err := d.ensureGoalWorktree(ctx, domainID, q.GoalID, gitURL, gitCredentials, defaultBranch)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("prepare workdir: %v", err))
		return
	}
	// Worktree cleanliness (DESIGN.md §4): a run must not start on a
	// worktree carrying changes nobody can account for — the daemon would
	// sweep a human's manual edits into the goal's commits. AGENTWORK.md
	// (platform-injected) and the domain-declared excludes are EXPECTED; a
	// cancelled previous run's leftovers are attributable (the checkpoint
	// resume model relies on them). Anything else dirty at run start parks
	// the goal in review for the human.
	//
	// Two escape hatches keep this from deadlocking:
	//   - a prior cancelled run → the dirt is that run's leftovers (resume);
	//   - the goal was just REJECTED → the dirt is what the agent must fix
	//     this round (the human told it to, via the review decision note) —
	//     blocking the run would trap the goal in park→reject forever.
	checks, timeout, baseline, checksFrozen := d.loadDomainChecks(ctx, domainID)
	if dirty := unattributedDirty(ctx, runRowWorkdir, checks.Excludes); dirty != "" {
		var priorCancelled int
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND status='cancelled'`, q.GoalID).Scan(&priorCancelled)
		var handoff string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT handoff_note FROM goal WHERE id=?`, q.GoalID).Scan(&handoff)
		justRejected := strings.HasPrefix(handoff, "Review decision: reject")
		if priorCancelled == 0 && !justRejected {
			reason := "worktree 有未归因的改动（可能是手动编辑）：\n" + dirty + "\n请检查 worktree 后批准继续或驳回。"
			if err := d.goalSvc.ParkForManualReview(ctx, q.GoalID, reason); err != nil {
				log.Printf("daemon: park %s for manual review: %v", q.GoalID, err)
			}
			// The run is parked WITHOUT the cancelled auto-retry path: the goal
			// is already in review (park set it) and a retry would re-park and
			// burn attempts. Stamp the run terminal directly — no Finish, no
			// reconcile (the goal layer already moved).
			if _, err := d.st.DB().ExecContext(ctx,
				`UPDATE run SET status='cancelled', finished_at=? WHERE id=?`, nowStr(), q.RunID); err != nil {
				log.Printf("daemon: park run %s terminal: %v", q.RunID, err)
			}
			return
		}
		log.Printf("daemon: worktree dirty for %s but attributable (cancelled leftovers or reject round) — continuing", q.GoalID)
	}

	// Environment readiness BEFORE the agent starts (决策 3-1, the setup half):
	// the acceptance policy's setup commands (dependency installs) prepare the
	// verification environment — but the agent needs the SAME environment to
	// self-verify while working (a pytest it cannot import is a blind run).
	// Run setup up front (idempotent; the verification stage re-runs it to
	// guarantee the judging environment is fresh). A failed setup here is an
	// environment failure — the run fails with that attribution, the retry
	// chain applies as usual.
	if checksFrozen && len(checks.Setup) > 0 {
		setupReport, ok := runSetupOnly(ctx, runRowWorkdir, checks, timeout)
		if !ok {
			d.finishRun(ctx, q, "failed", "environment setup failed:\n"+setupReport)
			return
		}
	}

	// The run's diff baseline: guards and evidence measure this run's changes
	// as baseSHA..HEAD (the agent may commit itself, and the daemon commits
	// leftover work at run end — both land in HEAD).
	baseSHA := strings.TrimSpace(mustGitRun(ctx, runRowWorkdir, "rev-parse", "HEAD"))

	// Inject the agent's identity + team roster / squad briefing into the
	// workdir so the agent subprocess discovers who it is and who it can hand
	// off to (AGENTWORK.md).
	roster := d.buildAgentGuide(ctx, q.AgentID)
	// Squad briefing is judged DYNAMICALLY — the run's agent must be the
	// goal's squad owner and its CURRENT leader. A leader mentioned by name
	// (mention://agent/<leader>) gets the same operating protocol as a leader
	// run triggered by assignment; authority and protocol stay consistent.
	briefing := ""
	if squadID, isLeader := d.leaderSquadFor(ctx, q.GoalID, q.AgentID); isLeader && squadID != "" {
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
		"AGENTWORK_GOAL_ID="+q.GoalID, // product-plane id (CLI comments/handoff)
		"AGENTWORK_RUN_ID="+q.RunID,   // execution-plane id
		"AGENTWORK_AGENT_ID="+q.AgentID,
	)
	// M4-B: issue-sourced goals carry the issue identity so the agent can
	// reply via `agentwork-cli issue comment` (the platform owns the token);
	// the LIVE issue comments are fetched at run start and injected into the
	// prompt — the issue stays the source of truth, nothing is stored.
	var issueRepo, issueNumber string
	var issueComments []issue.Comment
	if sourceRef != "" && gitCredentials != "" {
		if provider, repo, num, ok := issue.ParseSourceRef(sourceRef); ok {
			issueRepo, issueNumber = repo, strconv.Itoa(num)
			if client, err := issue.NewProvider(provider, gitCredentials); err == nil {
				if comments, err := client.ListComments(ctx, repo, num); err == nil {
					issueComments = comments
				} else if err != nil {
					log.Printf("daemon: issue comments for %s: %v", sourceRef, err)
				}
			}
		}
	}
	if issueRepo != "" {
		taskEnv = append(taskEnv,
			"AGENTWORK_ISSUE_REPO="+issueRepo,
			"AGENTWORK_ISSUE_NUMBER="+issueNumber,
		)
	}

	// Execution-environment proxy (DESIGN.md 决策 4-8): EVERY run gets the
	// same execution model — the worktree lives on the client side and is
	// reached through the ACP fs/terminal capabilities the client declares
	// in the handshake. stdio and remote agents are treated identically:
	// one mechanism, one behaviour, and the handler path is exercised by
	// every run's traffic instead of only remote ones. (A stdio agent whose
	// local tools operate on its cwd — the worktree — is unaffected: same
	// directory, same result.) Leftover terminals are killed
	// unconditionally at session close (defer).
	env := newRunEnvironment(q.RunID, q.GoalID, q.AgentID, runRowWorkdir, serverURL)
	defer env.tm.cleanup()

	// Workspace MCP server (DESIGN.md 决策 4-8): every run advertises its
	// workspace as an MCP server over HTTP at session/new — agents that do
	// not delegate tools to client fs/terminal RPCs (opencode's tools are
	// local by design) still get read_file/write_file/run_command bound to
	// THIS run's worktree + environment. Registered for the run's
	// lifetime; the /mcp/{runID} route resolves it.
	mcpExec := mcp.NewExecutor(runRowWorkdir, env.runEnv(nil), env.tm)
	mcpExec.SetCollaboration(q.GoalID, q.AgentID, q.RunID, d.commentSvc, d.goalSvc, d.runSvc, d.agentSvc, d.squadSvc)
	d.mu.Lock()
	d.mcpExecs[q.RunID] = mcpExec
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.mcpExecs, q.RunID)
		d.mu.Unlock()
	}()

	// Open the transport (stdio/ws/tcp); the backend speaks the protocol.
	spec := runtime.Spec{
		Transport:  transport,
		Executable: execPath,
		Args:       args,
		Endpoint:   endpoint,
		Env:        rtEnv,
		Cwd:        runRowWorkdir, // the goal's worktree — see Spec.Cwd
	}
	conn, err := runtime.Open(ctx, spec, taskEnv)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("open transport: %v", err))
		return
	}

	// Prompt the run: title + description + handoff/wakeup note, plus the
	// issue comments fetched at run start (M4-B: the issue is the
	// human-agent dialogue channel — the agent starts with the latest
	// conversation, nothing stored).
	// A REVIEW run (triggered by the platform's system review-request
	// comment) must NOT receive the goal's task description — it would
	// execute the task instead of reviewing it (a live failure: the opencode
	// reviewer re-created the file the leader had just written). Its prompt
	// is the review instruction only; the worktree, the comment feed
	// (injected below) and the workspace guidance give it everything else.
	prompt := buildPrompt(title, desc, handoff)
	if reviewRun {
		prompt = "你是审查者（reviewer）。请审查本次改动（squad 规矩：成员写完代码后由 reviewer 审查）。\n\n" +
			"只提意见，不要修改任何文件，不要执行任务本身。\n" +
			"审查工作区中的改动（diff、测试、质量），然后把你的意见用 agentwork_goal_comment 工具发到评论区（供审批人参考）。\n" +
			"意见要具体：问题、风险、改进建议。如果改动没问题，明确说明。"
	}

	// Workspace guidance (DESIGN.md 决策 4-8), injected for EVERY run — one
	// execution model for stdio and remote alike. The worktree lives on the
	// client side and is reached through the ACP fs/terminal capabilities
	// the client declared during the handshake. The guidance names only the
	// ACP protocol capabilities (deterministic); the agent maps them to its
	// own tools — its toolset is its own business, agentwork states the
	// environment facts and the collaboration contract.
	prompt += workspaceGuidance(runRowWorkdir)

	// The domain's acceptance policy in NL (the "what counts as done" the
	// OWNER defined) — the agent works toward it instead of finding out at
	// verification time. Only the NL intent is injected; the compiled checks
	// (verify commands / guard patterns) stay invisible — an agent that sees
	// the exact patterns can satisfy the check instead of the intent
	// (triangle separation: define stays with the human, execute with the
	// agent, judge with the machine+human).
	var policyText string
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT d.policy_text FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, q.GoalID).Scan(&policyText)
	if s := strings.TrimSpace(policyText); s != "" {
		prompt += "\n\n## 验收策略（这个域的完成标准，由域所有者定义）\n" + s +
			"\n\n机器会按此验收你的改动——干活时对照它，避免返工。"
	}

	// The verification-failure feedback loop: a RETRY run (attempt > 1, the
	// previous run failed machine verification) must know WHY it failed —
	// otherwise it blindly re-runs the same work and likely fails again.
	// The last failed run's report (verify/guards output) is injected with
	// an explicit "fix, don't redo" framing.
	if q.Attempt > 1 {
		var lastFail string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT result_summary FROM run WHERE goal_id=? AND status='failed' ORDER BY finished_at DESC LIMIT 1`,
			q.GoalID).Scan(&lastFail); err == nil && strings.TrimSpace(lastFail) != "" {
			prompt += "\n\n## 上一轮失败原因（机器验证未通过——据此修改现有代码，不要从零重做）\n" + truncateIn(lastFail, 1500)
		}
	}

	prompt += d.commentsInjection(ctx, q.GoalID)
	if len(issueComments) > 0 {
		var b strings.Builder
		b.WriteString("\n\n## Issue 最新交流（来自 GitHub）\n")
		for _, cm := range issueComments {
			fmt.Fprintf(&b, "- %s：%s\n", cm.User.Login, truncateIn(cm.Body, 300))
		}
		prompt += b.String()
	}
	// Mention-cycle hint (soft tier): the goal's agent-triggered churn is
	// above the hint threshold — tell the agents to stop circular handoffs
	// instead of perpetuating them. (The hard tier fails the goal at the
	// trigger site.)
	if n, err := d.agentTriggeredRunCount(ctx, q.GoalID); err == nil && n > service.MaxMentionHints {
		prompt += fmt.Sprintf("\n\n⚠️ 协作警告：agent 之间已互相转移任务 %d 次。请不要再把任务转给其他 agent——自己完成剩余工作，或结束本轮等待人工处理。", n)
	}

	backend, err := d.protoReg.Get(provider)
	if err != nil {
		conn.Close()
		d.failRun(ctx, q, fmt.Sprintf("provider %q: %v", provider, err))
		return
	}
	// maxRunDuration (DESIGN.md §4, decision 2-6): the run's total time
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
	// Register the run's cancel so a handoff can cut it (goal:assigned →
	// onGoalAssigned); unregister when the run finishes. The same cancel the
	// idle watchdog fires.
	d.mu.Lock()
	d.runCancels[q.RunID] = promptCancel
	delete(d.runCancelReasons, q.RunID) // fresh run — no stale reason
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.runCancels, q.RunID)
		delete(d.runCancelReasons, q.RunID)
		d.mu.Unlock()
	}()
	// Post-register self-check: a handoff that landed in the claim→register
	// window stamped this run terminal in the DB (onGoalAssigned found no
	// registered cancel to cut). Honor the stamp — self-cancel so the new
	// owner's run claims immediately instead of waiting out this one. The
	// stamp is the only concurrent writer of status besides finishRun, and
	// finishRun runs after this check, so there is no race.
	var registeredStatus string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT status FROM run WHERE id=?`, q.RunID).Scan(&registeredStatus)
	if registeredStatus != "running" {
		log.Printf("daemon: run %s terminal-stamped before registration — self-cancelling", q.RunID)
		d.mu.Lock()
		d.runCancelReasons[q.RunID] = "handoff"
		d.mu.Unlock()
		promptCancel()
	}
	go d.runIdleWatchdog(promptCtx, &lastActivity, &inFlightTools, promptCancel, q.RunID, env.tm.activeCount)

	run, err := backend.Execute(promptCtx, proto.ExecuteSpec{
		Conn:          conn,
		Cwd:           runRowWorkdir,
		Prompt:        prompt,
		ClientHandler: env,
		McpServers: []acp.McpServer{{
			Type: "http",
			Name: "agentwork",
			URL:  serverURL + "/mcp/" + q.RunID,
		}},
	})
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

		// The run's REPORT is its last assistant message — the agent's final
		// summary (what it did + how it verified), NOT the full transcript
		// (which opens with the agent's thinking). The approval card, the
		// comment feed, and the deliver note all read this; a transcript
		// opening made Feishu's approval card show "I'm the worker agent…"
		// instead of the work. Falls back to the backend output.
		var report string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT content FROM chat_message WHERE run_id=? AND role='assistant' AND content != '' ORDER BY created_at DESC LIMIT 1`,
			q.RunID).Scan(&report); err != nil || strings.TrimSpace(report) == "" {
			report = result.Output
		}

		if domainID != "" {
			// Make the agent's work durable on the goal branch (the agent is
			// guided to commit; the daemon guarantees it — deliver merges the
			// branch, and uncommitted work would deliver nothing). The
			// domain's declared excludes (checks.excludes, compiled + owner-
			// confirmed) keep dependency dirs out of the branch.
			// A REVIEW run is exempt (DESIGN.md 决策 4-4): its product is the
			// opinion, not code — review artifacts must not merge into the
			// goal branch. Leftovers (if the reviewer edited anyway) fail the
			// next run's dirty check into a human park — a safe fallback.
			if !reviewRun {
				if err := commitRunChanges(ctx, runRowWorkdir, d.domainGitIdentity(ctx, domainID), checks.Excludes); err != nil {
					d.finishRun(ctx, q, "failed", "commit run changes: "+err.Error())
					return
				}
			}
			// Machine verification (DESIGN.md §4/§5 (§9 invariant 14)): the
			// domain's verify commands run BEFORE the run is finished. A red
			// verify ends the run failed → retry chain. The goal layer only
			// ever sees 'completed' runs that passed.
			//
			// An UNFROZEN policy (checks_compiled_at empty — the owner never
			// confirmed the compiled checks) runs NOTHING: no setup/verify/
			// guards against an unconfirmed definition, and no gate evaluation
			// (the goal layer forces the human checkpoint instead). Evidence
			// still carries the diff + agent summary for that checkpoint.
			verifyReport, guardReport := "", ""
			if checksFrozen {
				verifyReport, ok, policyIssue := runVerification(ctx, runRowWorkdir, checks, timeout)
				if !ok {
					// Objective policy defect (POSIX exit 127 — missing command /
					// script)? Flag it once — the owner fixes the policy instead
					// of the agent burning retries against an impossible check.
					if policyIssue {
						d.annotatePolicyIssue(ctx, q.GoalID)
					}
					d.finishRun(ctx, q, "failed", "verification failed:\n"+verifyReport)
					return
				}
				// Structural guards on the diff (DESIGN.md §5.1), measured as
				// baseSHA..HEAD — the run's own changes. git status would be empty
				// here: the daemon just committed the agent's work (and the agent
				// may have committed itself), so the worktree is clean.
				guardReport, ok = checkGuards(ctx, runRowWorkdir, baseSHA, checks, baseline)
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
			}
			// Evidence bundle for the approval card (decision 2-3).
			ev := buildEvidence(ctx, runRowWorkdir, baseSHA, report, verifyReport, guardReport)
			if _, err := d.st.DB().ExecContext(ctx, `UPDATE run SET evidence=? WHERE id=?`, ev, q.RunID); err != nil {
				log.Printf("daemon: store evidence for run %s: %v", q.RunID, err)
			}
		}
		d.finishRunOK(ctx, q, report)
	case proto.StatusCancelled:
		// decision 2-6 + the "stuck active with no run" hole: a cancelled run
		// does NOT fail the goal, and does NOT consume attempt credit — the
		// requeue keeps the SAME attempt (a timeout is not a machine failure).
		// The convergence rule is separate: only the FIRST cancellation gets an
		// automatic retry; a second consecutive stall is systemic (the agent
		// keeps hanging) and surfaces to the owner instead of looping.
		// Only IDLE-WATCHDOG cancellations count toward the convergence rule —
		// the worktree-dirty park, handoff cuts, approval cuts, and human
		// cancels also mark runs cancelled (without the watchdog summary) and
		// must not consume the single automatic retry.
		// Structured cancellation reason (no string-matching semantics): the
		// code is stamped on the run row + carried in the event; the summary
		// text is display only.
		code := d.takeCancelReason(q.RunID)
		if code == "" {
			code = "timeout" // maxRunDuration deadline — no retry (决策 2-6)
		}
		display := map[string]string{
			"idle_watchdog": "idle watchdog",
			"handoff":       "handoff",
			"stopped":       "stopped", // human-initiated (StopRun / Cancel)
			"timeout":       "timeout",
		}[code]
		summary := display + ": " + result.Output
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE run SET cancel_reason=? WHERE id=?`, code, q.RunID); err != nil {
			log.Printf("daemon: stamp cancel_reason %s: %v", q.RunID, err)
		}
		var priorCancelled int
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND status='cancelled' AND cancel_reason='idle_watchdog'`, q.GoalID).Scan(&priorCancelled)
		if priorCancelled == 0 && code == "idle_watchdog" {
			var isLeader int
			var squadID string
			_ = d.st.DB().QueryRowContext(ctx, `SELECT is_leader_run, squad_id FROM run WHERE id=?`, q.RunID).Scan(&isLeader, &squadID)
			if err := d.runSvc.EnqueueExisting(ctx, q.GoalID, q.AgentID, q.Attempt, isLeader != 0, squadID); err != nil {
				log.Printf("daemon: requeue cancelled run %s: %v", q.RunID, err)
			}
		}
		d.finishRun(ctx, q, "cancelled", summary)
		// Surface the stall so the notify layer can tell the owner a task
		// stalled (cancelled runs leave the goal active with no pending run —
		// the human decides, per decision 2-6).
		d.bus.Publish(ctx, events.Event{Topic: "run:cancelled", Payload: map[string]any{
			"run_id": q.RunID, "goal_id": q.GoalID, "reason": summary, "reason_code": code,
		}})
	case proto.StatusFailed, proto.StatusAborted:
		d.finishRun(ctx, q, "failed", result.Output)
	}
}

// runProcessorTask executes a platform-internal processor run: opens the
// agent's transport, sends the run's fixed prompt, drains events, and then
// collects the FILE-based result — the compiled checks.json + strength.txt —
// from the run workdir and stores it on the associated domain in an UNFROZEN
// state (checks_compiled_at stays ”), publishing domain:compiled so the
// frontend can show the owner confirmation card. Structured output is read
// from files, never parsed from agent stdout (DESIGN.md §5.3, §9).
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

	// Workdir: compile runs work ON THE REAL REPO — a worktree of the domain's
	// shared bare repo, so the processor compiles against what the repo
	// actually is, not a guess from an empty scratch dir (the regression: an
	// empty dir produced "pip install -r requirements.txt" for a zero-
	// dependency repo and every verification failed). Intake runs keep the
	// scratch dir (no repo involved).
	runRowWorkdir := filepath.Join(workspaceRoot(), "proc", q.RunID)
	if runType == "compile" {
		var gitURL, gitCredentials, defaultBranch string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT git_url, git_credentials, default_branch FROM domain WHERE id=?`, domainID).
			Scan(&gitURL, &gitCredentials, &defaultBranch)
		if gitURL == "" {
			d.failProcessorRun(ctx, q, "compile run: domain has no git_url")
			return
		}
		unlock := d.lockDomain(domainID)
		if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
			unlock()
			d.failProcessorRun(ctx, q, "prepare compile repo: "+err.Error())
			return
		}
		repo := domainRepoPath(domainID)
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", runRowWorkdir, "origin/"+defaultBranch).CombinedOutput(); err != nil {
			unlock()
			d.failProcessorRun(ctx, q, "compile worktree add: "+err.Error()+": "+string(out))
			return
		}
		unlock()
		defer func() {
			unlock := d.lockDomain(domainID)
			_, _ = exec.CommandContext(context.Background(), "git", "-C", repo, "worktree", "remove", "--force", runRowWorkdir).CombinedOutput()
			unlock()
		}()
	} else if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
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
	serverURL := "http://" + net.JoinHostPort("127.0.0.1", port)
	taskEnv = append(taskEnv, "AGENTWORK_SERVER_URL="+serverURL)

	conn, err := runtime.Open(ctx, runtime.Spec{
		Transport: transport, Executable: execPath, Args: args, Endpoint: endpoint, Env: rtEnv,
		Cwd: runRowWorkdir, // the processor's scratch dir — see Spec.Cwd
	}, taskEnv)
	if err != nil {
		d.failProcessorRun(ctx, q, "open transport: "+err.Error())
		return
	}
	defer conn.Close()

	// Unified execution model (DESIGN.md 决策 4-8): a processor run gets the
	// same client execution environment + MCP workspace server + Workspace
	// contract as a worker run. Without the client handler the ACP handshake
	// declares no fs/terminal capabilities and the processor agent's
	// write/shell tools are rejected ("agent→client RPC not configured") —
	// the compile run cannot land its checks.json artifact (a live failure).
	env := newRunEnvironment(q.RunID, "", q.AgentID, runRowWorkdir, serverURL)
	defer env.tm.cleanup()
	d.mu.Lock()
	mcpExec := mcp.NewExecutor(runRowWorkdir, env.runEnv(nil), env.tm)
	mcpExec.SetCollaboration(q.GoalID, q.AgentID, q.RunID, d.commentSvc, d.goalSvc, d.runSvc, d.agentSvc, d.squadSvc)
	d.mcpExecs[q.RunID] = mcpExec
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.mcpExecs, q.RunID)
		d.mu.Unlock()
	}()
	prompt += workspaceGuidance(runRowWorkdir)

	backend, err := d.protoReg.Get(provider)
	if err != nil {
		d.failProcessorRun(ctx, q, "provider "+provider+": "+err.Error())
		return
	}
	// Time bounds, same as worker runs (a compile agent stuck on a slow pip
	// index was running UNBOUNDED — no maxRunDuration, no idle watchdog, so
	// the run sat "running" forever and the page showed 编译中… indefinitely;
	// a live 15+ minute hang on pypi.org). The domain's max_run_duration is
	// the budget (default 2h); the idle watchdog cuts silent stalls, and the
	// terminal manager's activeCount widens its window like a worker run.
	maxRunDuration := 7200
	if domainID != "" {
		var d2 int
		_ = d.st.DB().QueryRowContext(ctx, `SELECT max_run_duration FROM domain WHERE id=?`, domainID).Scan(&d2)
		if d2 > 0 {
			maxRunDuration = d2
		}
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var inFlightTools atomic.Int32
	procCtx, procCancel := context.WithTimeout(ctx, time.Duration(maxRunDuration)*time.Second)
	defer procCancel()
	go d.runIdleWatchdog(procCtx, &lastActivity, &inFlightTools, procCancel, q.RunID, env.tm.activeCount)

	run, err := backend.Execute(procCtx, proto.ExecuteSpec{
		Conn:          conn,
		Cwd:           runRowWorkdir,
		Prompt:        prompt,
		ClientHandler: env,
		McpServers: []acp.McpServer{{
			Type: "http", Name: "agentwork", URL: serverURL + "/mcp/" + q.RunID,
		}},
	})
	if err != nil {
		d.failProcessorRun(ctx, q, "execute: "+err.Error())
		return
	}
	for ev := range run.Events {
		lastActivity.Store(time.Now().UnixNano())
		d.persistEvent(ctx, q.RunID, ev)
		d.trackToolInflight(&inFlightTools, ev)
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
		// the previous one wholesale — DESIGN.md §5.3), and resets the
		// freeze stamp: the domain returns to the pending-confirmation state
		// so the owner's confirmation card reappears with the NEW product.
		// (Regression: the old UPDATE was gated on checks_compiled_at='',
		// which made a recompile AFTER freezing a silent no-op — the new
		// product was discarded and runs kept verifying with the old policy.)
		//
		// The evolution-metrics baseline (decision 2-15) is recorded alongside
		// (metrics.json — test count / coverage the processor measured at
		// compile time). Only the FIRST compile stamps the baseline: later
		// recompiles refresh the policy, not the evolution baseline.
		baseline := "{}"
		if raw, err := os.ReadFile(filepath.Join(runRowWorkdir, "metrics.json")); err == nil {
			var m struct {
				TestCount int     `json:"test_count"`
				Coverage  float64 `json:"coverage"`
			}
			if json.Unmarshal(raw, &m) == nil && (m.TestCount > 0 || m.Coverage > 0) {
				b, _ := json.Marshal(map[string]any{"test_count": m.TestCount, "coverage": m.Coverage})
				baseline = string(b)
			}
		}
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE domain SET checks=?, verification_strength=?, checks_compiled_at='',
			        metrics_baseline=CASE WHEN metrics_baseline='{}' OR metrics_baseline='' THEN ? ELSE metrics_baseline END
			 WHERE id=?`,
			string(checksJSON), strength, baseline, domainID); err != nil {
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

// workspaceGuidance is the Workspace contract (DESIGN.md 决策 4-8) injected
// into EVERY run's prompt — worker AND processor alike (the unified model:
// every run registers the client handler, declares capabilities, and gets
// this section; a processor run without it cannot write its file artifact —
// a live failure: the compile agent's write/shell tools were rejected with
// "agent→client RPC not configured" and checks.json never landed). It names
// the platform's own tools (deterministic); the agent's own toolset is its
// business — the contract is the boundary.
func workspaceGuidance(workdir string) string {
	return fmt.Sprintf(`
## Workspace

Your workspace lives on the PLATFORM machine — it is not your environment:

- Workspace root: %s
- It contains the repository code and AGENTWORK.md, the coordination guide — read it first
- COLLABORATE through the platform's MCP collaboration tools (agentwork_goal_comment / agentwork_goal_assign / agentwork_goal_list / agentwork_agent_list / agentwork_squad_list) — the coordination contract lives in AGENTWORK.md
- ACCESS THE WORKSPACE ONLY THROUGH THE PLATFORM'S CHANNELS:
  * MCP server "agentwork" (advertised at session start) — its tools operate on the workspace: agentwork_read_file (read a file), agentwork_write_file (write a file), and the command trio agentwork_terminal_create → agentwork_terminal_output → agentwork_terminal_release (commands are ASYNC: create returns a terminal id immediately, poll output until exited=true passing the returned cursor back, then release to clean up)
  * Client capabilities over ACP — fs/read_text_file, fs/write_text_file, terminal/*
  Your own local file/shell tools operate on YOUR environment — NOT the workspace. On a remote runtime your local tools cannot reach the workspace at all; locally they only happen to work when the working directory points at it
- Commands that touch the workspace run through the platform's execution channel (agentwork_terminal_create or terminal/*), on the platform machine, with the workspace as their working directory
- Verification, review and delivery read only what you wrote through these channels
`, workdir)
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
// push the goal to done when the goal is assigned to THIS squad (DESIGN.md
// §9). A guest @mentioned squad gets the "do NOT change status" briefing.
func (d *Daemon) goalOwnsSquadStatus(ctx context.Context, goalID, squadID string) bool {
	var at, aid string
	err := d.st.DB().QueryRowContext(ctx, `SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).Scan(&at, &aid)
	if err != nil {
		return false
	}
	return at == "squad" && aid == squadID
}

// leaderSquadFor reports the squad the goal belongs to when the given agent
// is its CURRENT leader (dynamic — judged at run time, not from a static
// run mark).
func (d *Daemon) leaderSquadFor(ctx context.Context, goalID, agentID string) (string, bool) {
	var atype, aid string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).Scan(&atype, &aid)
	if err != nil || atype != "squad" || aid == "" {
		return "", false
	}
	var leaderID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT leader_id FROM squad WHERE id=?`, aid).Scan(&leaderID); err != nil {
		return "", false
	}
	return aid, leaderID == agentID
}

// annotatePolicyIssue posts ONE system comment flagging an objective
// acceptance-policy defect (a verify command/script that does not exist —
// POSIX exit 127) — deduped per goal so the retry chain does not spam the
// same finding. The owner sees it in the feed and fixes the policy; the run
// still fails normally (a report is not a waiver).
func (d *Daemon) annotatePolicyIssue(ctx context.Context, goalID string) {
	var n int
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system' AND content LIKE '疑似验收策略问题%'`, goalID).Scan(&n)
	if n > 0 {
		return
	}
	content := "⚠️ 疑似验收策略问题：验证命令/脚本不存在（POSIX exit 127）——请检查域的验收策略，修正后重开任务；agent 无法绕过该检查。"
	_, _ = d.st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
		uuid.NewString(), goalID, content, nowStr())
}

// agentTriggeredRunCount counts the goal's runs triggered by AGENT-authored
// comments (the mention-churn signal; platform system triggers excluded).
func (d *Daemon) agentTriggeredRunCount(ctx context.Context, goalID string) (int, error) {
	var n int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run r JOIN comment c ON c.id = r.trigger_comment_id
		 WHERE r.goal_id=? AND c.author_type='agent'`, goalID).Scan(&n)
	return n, err
}

// commentsInjection renders the goal's FULL collaboration feed as the
// prompt's comment section — every comment, human AND agent authors, no
// count limit, in time order. This is the collaboration-chain guarantee
// (DESIGN.md 决策 4-6): an agent pulled in by another agent's mention must
// see what was asked of it — the earlier human-only + LIMIT 5 filter broke
// exactly that chain. Compression (决策 4-7, budget-based summarization) is
// designed but not implemented; full-feed injection is the guarantee until
// then.
func (d *Daemon) commentsInjection(ctx context.Context, goalID string) string {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT author_type, content FROM comment WHERE goal_id=?
		 ORDER BY created_at ASC`, goalID)
	if err != nil {
		log.Printf("daemon: load comments for run prompt (goal %s): %v", goalID, err)
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var at, content string
		if rows.Scan(&at, &content) == nil && strings.TrimSpace(content) != "" {
			b.WriteString("- " + at + "：" + content + "\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\n## Goal 评论（协作交流，完整 feed）\n" + b.String()
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
	guide := "Read AGENTWORK.md in the working directory first — it is the coordination guide for this run (team roster, how to collaborate via the agentwork_goal_* tools: comment with mentions, hand off, inspect)."
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
// mention:// URI format. See DESIGN.md §2 (coordination primitives).
func (d *Daemon) buildAgentGuide(ctx context.Context, selfAgentID string) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id, name, description FROM agent ORDER BY name`)
	if err != nil {
		log.Printf("daemon: build team roster: %v", err)
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("## Team & Coordination\n\n")
	b.WriteString("You coordinate through the MCP tools of the \"agentwork\" server (advertised at\n")
	b.WriteString(" session start) — structured side effects, no shell, no CLI:\n")
	b.WriteString("- agentwork_goal_comment: post on THIS goal (embed a mention to trigger a run)\n")
	b.WriteString("- agentwork_goal_assign: hand this goal to another agent/squad\n")
	b.WriteString("- agentwork_goal_list / agentwork_agent_list / agentwork_squad_list: inspect\n")
	b.WriteString("Do NOT edit files to communicate intent — structured side effects are the only way.\n\n")

	b.WriteString("### Hand off the current goal\n")
	b.WriteString("- Call agentwork_goal_assign (assignee_type=agent|squad, note = scoping instruction).\n")
	b.WriteString("  END YOUR TURN immediately after — the platform terminates your run (its result\n")
	b.WriteString("  no longer affects the goal, and a live run would block the new owner's: the\n")
	b.WriteString("  goal executes serially). Do not keep working and do not wait for the new owner.\n\n")

	b.WriteString("### Pull in another agent via @mention\n")
	b.WriteString("- Call agentwork_goal_comment with a structured mention:\n")
	b.WriteString("  [@Name](mention://agent/<AGENT-UUID>) <what you want>\n")
	b.WriteString("- The mention MUST be a structured Markdown link with a mention:// URI; bare")
	b.WriteString(" `@handle` prose does NOT trigger anything. Resolve the UUID first with")
	b.WriteString(" agentwork_agent_list. mention://agent/<uuid> triggers a new run on that agent")
	b.WriteString(" for THIS goal; mention://squad/<uuid> routes to the squad's leader.\n")
	b.WriteString(" @all (mention://all/all) suppresses auto-trigger.\n\n")
	b.WriteString("### Collaboration is RELAY, not waiting\n")
	b.WriteString("- Runs on this goal execute SERIALLY: the goal has one worktree and one\n")
	b.WriteString("  running run at a time. Your run staying alive BLOCKS the agent you\n")
	b.WriteString("  mentioned — their run only starts when yours ends. Never wait inside\n")
	b.WriteString("  your run for another agent's result; it can never arrive.\n")
	b.WriteString("- If you depend on another agent's result: finish everything you can NOW,\n")
	b.WriteString("  mention them with exactly what you need, then END your turn. When they\n")
	b.WriteString("  finish, they mention you back — your next run starts fresh (attempt 1),\n")
	b.WriteString("  the full comment feed (including their handoff note and your own\n")
	b.WriteString("  previous report) is injected, and the worktree already contains their\n")
	b.WriteString("  committed work. Pick up from there.\n\n")

	b.WriteString("### Inspect\n")
	b.WriteString("- agentwork_goal_list / agentwork_agent_list / agentwork_squad_list — use\n")
	b.WriteString("  agent_list to get UUIDs for mentions and handoffs.\n\n")

	b.WriteString("### Team roster\n")
	b.WriteString("If a task falls outside your role, delegate it — mention the teammate whose")
	b.WriteString(" role best matches in a comment (see @mention above) so they pick it up on")
	b.WriteString(" this goal, or hand off the goal entirely.\n\n")
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
// (or idleToolWindow while a tool is in flight or a terminal command is
// running). Terminal polling (Agent→Client RPC) never appears on the event
// stream — an agent waiting on `npm test` is silent to the daemon, so an
// in-flight terminal widens the budget exactly like an in-flight tool.
// It ticks at window/2.
func (d *Daemon) runIdleWatchdog(parent context.Context, lastActivity *atomic.Int64, inFlightTools *atomic.Int32, cancel context.CancelFunc, runID string, activeTerms func() int) {
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
			if inFlightTools.Load() > 0 || (activeTerms != nil && activeTerms() > 0) {
				threshold = idleToolWindow
			}
			last := time.Unix(0, lastActivity.Load())
			if time.Since(last) < threshold {
				continue
			}
			log.Printf("daemon: idle watchdog firing for run %s (silent %s), force-stopping", runID, time.Since(last).Round(time.Second))
			d.mu.Lock()
			d.runCancelReasons[runID] = "idle_watchdog"
			d.mu.Unlock()
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
