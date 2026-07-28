// Package daemon dispatches task_queue rows to agent runtimes. MVP uses the
// per-task subprocess model (like multica): each task run spawns a fresh ACP
// agent process via acp.ConnectStdio, runs one Prompt, then tears it down.
// There is no long-lived per-agent server process. The agent.status/pid and
// session table columns are retained for a future ws/tcp long-lived model but
// are weakly used under this model.
//
// Concurrency is per-agent: each agent has a worker goroutine with a semaphore
// sized to agent.max_concurrent, so one agent's tasks run in parallel up to its
// limit while different agents are independent. The daemon is embedded in the
// agentwork-daemon binary for MVP.
package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/runtime"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"github.com/google/uuid"
)

// maxAttempts bounds per-task retries. A task that fails this many times is
// left in task_queue status='failed' for human inspection.
const maxAttempts = 3

// dispatchTickInterval is how often the daemon scans task_queue for claimable
// rows. Sub-second keeps perceived latency low without hot-looping.
const dispatchTickInterval = 500 * time.Millisecond

// scheduleTickInterval is how often the daemon scans schedule for due firings.
// Schedule precision to the second is enough; this only bounds how late a
// firing can be relative to its next_run_at.
const scheduleTickInterval = 5 * time.Second

// workerQueueDepth bounds how many task_queue rows one agent's worker holds
// before back-pressuring the dispatcher. Generous for local single-user use.
const workerQueueDepth = 64

// defaultListenAddr is used when no addr is configured.
const defaultListenAddr = ":7373"

// Daemon owns per-agent workers and the task dispatch loop.
type Daemon struct {
	st      *store.Store
	bus     *events.Bus
	addr    string // HTTP listen address, injected into agent env so agentwork-cli can reach the server
	taskSvc *service.TaskService

	mu      sync.Mutex
	workers map[string]*agentWorker // agentID → per-agent scheduler
	stopped bool
	ctx     context.Context // long-lived daemon context (set in Run)
}

// agentWorker schedules one agent's tasks with a concurrency semaphore.
type agentWorker struct {
	agentID   string
	sem       chan struct{}    // capacity = max_concurrent
	queue     chan *queuedRow  // buffered pending tasks
	ctx       context.Context
	cancel    context.CancelFunc // cancels this worker when the agent is deleted
	daemonCtx context.Context     // daemon lifetime; passed to runTask so agent delete doesn't interrupt in-flight tasks
	run       func(context.Context, *queuedRow) // bound to Daemon.runTask
}

func New(st *store.Store, bus *events.Bus, addr string, taskSvc *service.TaskService) *Daemon {
	d := &Daemon{st: st, bus: bus, addr: addr, taskSvc: taskSvc, workers: make(map[string]*agentWorker)}
	bus.Subscribe("agent:created", d.onAgentCreated)
	bus.Subscribe("agent:deleted", d.onAgentDeleted)
	return d
}

// Run starts the dispatch loop. Blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	d.recoverStuckRunning(ctx)
	dispatchTick := time.NewTicker(dispatchTickInterval)
	scheduleTick := time.NewTicker(scheduleTickInterval)
	defer dispatchTick.Stop()
	defer scheduleTick.Stop()
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
		}
	}
}

// recoverStuckRunning reclaims task_queue rows left in status='running' by a
// previous daemon process that died without finishing them. Without this, a
// kill/restart orphans every in-flight task forever — claimQueued only picks
// status='queued'. We reset both task_queue and task back to queued so the
// dispatch loop re-claims them. started_at is cleared so the next claim
// stamps a fresh one.
func (d *Daemon) recoverStuckRunning(ctx context.Context) {
	tx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		log.Printf("daemon: recover stuck running: %v", err)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE task_queue SET status='queued', started_at='' WHERE status='running'`)
	if err != nil {
		log.Printf("daemon: recover stuck running: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE task SET status='queued' WHERE id IN (SELECT task_id FROM task_queue WHERE status='queued' AND started_at='') AND status='running'`); err != nil {
			log.Printf("daemon: recover stuck running (task): %v", err)
			return
		}
		log.Printf("daemon: recovered %d stuck running task(s)", n)
	}
	if err := tx.Commit(); err != nil {
		log.Printf("daemon: recover stuck running commit: %v", err)
	}
}

// ── agent worker lifecycle ──

// onAgentCreated registers a per-agent worker. Under the per-task subprocess
// model, creating an agent does NOT launch a process; the process is spawned
// per task at run time. The payload is the full service.Agent published by
// AgentService.Create.
func (d *Daemon) onAgentCreated(ctx context.Context, e events.Event) {
	a, ok := e.Payload.(service.Agent)
	if !ok {
		return
	}
	d.ensureWorker(a.ID, a.MaxConcurrent)
	log.Printf("daemon: worker ready for agent %s", a.ID)
}

// ensureWorker creates the per-agent worker if it doesn't exist yet. The
// worker binds to the daemon's long-lived context, not any request context.
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
		queue:     make(chan *queuedRow, workerQueueDepth),
		daemonCtx: d.ctx,
		run:       d.runTask,
	}
	w.ctx, w.cancel = context.WithCancel(d.ctx)
	d.workers[agentID] = w
	go w.loop()
	return w
}

// loop drains the worker's queue, running each task under the concurrency
// semaphore. Tasks run in their own goroutine so the loop stays free to accept
// the next queued item.
//
// w.ctx is cancelled when the agent is deleted (stops the drain) or the daemon
// shuts down. But runTask receives the daemon's long-lived context, not w.ctx:
// deleting an agent should not interrupt a task that is already running — it
// should only prevent new tasks from starting on this agent. The running task
// finishes, and its finishTask/failOrRequeue will then find no worker and
// requeue onto another agent (or leave it queued for manual reassignment).
func (w *agentWorker) loop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case q, ok := <-w.queue:
			if !ok {
				return
			}
			w.sem <- struct{}{} // acquire concurrency slot
			go func(q *queuedRow) {
				defer func() { <-w.sem }()
				defer func() {
					if r := recover(); r != nil {
						log.Printf("daemon: panic in runTask for task %s: %v", q.TaskID, r)
					}
				}()
				w.run(w.daemonCtx, q)
			}(q)
		}
	}
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
		// Stop the drain so no new tasks start on this agent. In-flight tasks
		// keep running on w.daemonCtx and finish normally; their finishTask
		// will find no worker and leave the task in its terminal status.
		w.cancel()
	}
	log.Printf("daemon: worker removed for agent %s", id)
}

func (d *Daemon) stopAll() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	// Worker loops exit via ctx cancellation from Run. Nothing else to tear
	// down: per-task subprocesses are owned by their runTask goroutines.
}

// ── task dispatch ──

// dispatchOnce claims one queued task_queue row and routes it to the agent's
// worker.
func (d *Daemon) dispatchOnce(ctx context.Context) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	q, err := d.claimQueued(ctx)
	if err != nil || q == nil {
		return
	}

	d.mu.Lock()
	w, ok := d.workers[q.AgentID]
	d.mu.Unlock()
	if !ok {
		// Agent has no worker (not created or deleted). Requeue and retry later.
		d.requeue(ctx, q, "agent not registered")
		return
	}
	select {
	case w.queue <- q:
	default:
		// Worker queue full; requeue and let the next tick retry.
		d.requeue(ctx, q, "worker queue full")
	}
}

// ── schedule dispatch ──

// dispatchSchedules fires every enabled schedule whose next_run_at is due,
// cloning a fresh task from the template and enqueueing it. Idempotent via the
// uq_schedule_run_planned unique index: the same (schedule_id, planned_at)
// can never fire twice, even across restarts or overlapping ticks.
func (d *Daemon) dispatchSchedules(ctx context.Context) {
	nowStr := now()
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT id, title_template, description, assignee_id, cron_expression, timezone, next_run_at
		 FROM schedule
		 WHERE enabled=1 AND next_run_at != '' AND next_run_at <= ?`, nowStr)
	if err != nil {
		log.Printf("daemon: schedule query: %v", err)
		return
	}
	var due []scheduleDueRow
	for rows.Next() {
		var r scheduleDueRow
		if err := rows.Scan(&r.ScheduleID, &r.TitleTemplate, &r.Description, &r.AssigneeID, &r.CronExpression, &r.Timezone, &r.NextRunAt); err != nil {
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
	ScheduleID, TitleTemplate, Description, AssigneeID, CronExpression, Timezone, NextRunAt string
}

// fireSchedule handles one due schedule: clone task, record run, advance
// next_run_at. Idempotency is enforced by the uq_schedule_run_planned unique
// index inside the transaction — if a concurrent tick already inserted this
// planned_at, the schedule_run insert fails, the whole tx rolls back (no orphan
// task), and we just advance next_run_at. Errors are logged but do not stop the
// tick.
func (d *Daemon) fireSchedule(ctx context.Context, r scheduleDueRow) {
	plannedAt := r.NextRunAt

	taskID := uuid.NewString()
	ts := now()
	tx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		log.Printf("daemon: schedule %s begin tx: %v", r.ScheduleID, err)
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task (id,title,description,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,'','agent',?,'queued','','system',?,?)`,
		taskID, r.TitleTemplate, r.AssigneeID, r.ScheduleID, ts); err != nil {
		log.Printf("daemon: schedule %s insert task: %v", r.ScheduleID, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_queue (id,task_id,agent_id,status,attempt,queued_at) VALUES (?,?,?,'queued',1,?)`,
		uuid.NewString(), taskID, r.AssigneeID, ts); err != nil {
		log.Printf("daemon: schedule %s insert task_queue: %v", r.ScheduleID, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schedule_run (id,schedule_id,task_id,planned_at,status,created_at) VALUES (?,?,?,?,'dispatched',?)`,
		uuid.NewString(), r.ScheduleID, taskID, plannedAt, ts); err != nil {
		// Unique index violation → a concurrent tick@tick already fired this
		// planned_at. Roll back the task/task_queue inserts (defer does this)
		// and just advance next_run_at so we don't loop on the same slot.
		log.Printf("daemon: schedule %s already fired at %s, skipping", r.ScheduleID, plannedAt)
		d.advanceScheduleNextRun(ctx, r, plannedAt)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("daemon: schedule %s commit: %v", r.ScheduleID, err)
		return
	}

	// Advance next_run_at anchored at plannedAt (not now) so a lagging clock
	// can't re-point at the slot that just fired.
	d.advanceScheduleNextRun(ctx, r, plannedAt)

	d.bus.Publish(ctx, events.Event{Topic: "schedule:fired", Payload: map[string]any{
		"schedule_id": r.ScheduleID, "task_id": taskID, "planned_at": plannedAt,
	}})
	log.Printf("daemon: schedule %s fired, created task %s", r.ScheduleID, taskID)
}

// advanceScheduleNextRun recomputes next_run_at from plannedAt and updates the
// schedule row. Anchoring at plannedAt (not now) keeps the cron on its grid.
func (d *Daemon) advanceScheduleNextRun(ctx context.Context, r scheduleDueRow, plannedAt string) {
	anchor, err := time.Parse(time.RFC3339Nano, plannedAt)
	if err != nil {
		// Fall back to now if the stored timestamp is unparseable.
		anchor = time.Now().UTC()
	}
	next, err := service.ComputeNextRun(r.CronExpression, r.Timezone, anchor)
	if err != nil {
		log.Printf("daemon: schedule %s advance cron: %v", r.ScheduleID, err)
		return
	}
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE schedule SET next_run_at=?, last_run_at=? WHERE id=?`,
		next.Format(time.RFC3339Nano), now(), r.ScheduleID); err != nil {
		log.Printf("daemon: schedule %s advance next_run_at: %v", r.ScheduleID, err)
	}
}

// queuedRow is a claimed task_queue row.
type queuedRow struct {
	QueueID string
	TaskID  string
	AgentID string
	Attempt int
}

// claimQueued atomically claims the oldest queued row whose agent is not
// crashed. The claim and the status flip happen in a single UPDATE … RETURNING
// so two concurrent ticks can never both grab the same row.
func (d *Daemon) claimQueued(ctx context.Context) (*queuedRow, error) {
	tx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var q queuedRow
	err = tx.QueryRowContext(ctx,
		`UPDATE task_queue
		 SET status='running', started_at=?
		 WHERE id = (
		   SELECT tq.id
		   FROM task_queue tq
		   JOIN agent a ON a.id = tq.agent_id
		   WHERE tq.status='queued' AND a.status != 'crashed'
		   ORDER BY tq.queued_at
		   LIMIT 1
		 )
		 RETURNING id, task_id, agent_id, attempt`, now()).
		Scan(&q.QueueID, &q.TaskID, &q.AgentID, &q.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task SET status='running' WHERE id=?`, q.TaskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &q, nil
}

// requeue returns a claimed row to queued status for later retry.
func (d *Daemon) requeue(ctx context.Context, q *queuedRow, reason string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE task_queue SET status='queued', started_at='' WHERE id=?`, q.QueueID); err != nil {
		log.Printf("daemon: requeue task %s: %v", q.TaskID, err)
	}
	log.Printf("daemon: requeued task %s (%s)", q.TaskID, reason)
}

// runTask spawns a fresh ACP subprocess for this task, runs one Prompt, drains
// events, and records the outcome.
func (d *Daemon) runTask(ctx context.Context, q *queuedRow) {
	// Load task + agent + runtime config.
	var title, desc, handoff, workdirBase, transport, execPath, argsJSON, endpoint, envJSON string
	var maxConcurrent int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT t.title, t.description, t.handoff_note, a.workdir_base,
		        r.transport, r.executable, r.args, r.endpoint, r.env, a.max_concurrent
		 FROM task t
		 JOIN agent a ON a.id = t.assignee_id
		 JOIN runtime r ON r.id = a.runtime_id
		 WHERE t.id = ?`, q.TaskID).
		Scan(&title, &desc, &handoff, &workdirBase, &transport, &execPath, &argsJSON, &endpoint, &envJSON, &maxConcurrent)
	if err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("load config: %v", err))
		return
	}

	// Ensure the per-agent worker exists (in case the agent:created event
	// hasn't been processed yet).
	d.ensureWorker(q.AgentID, maxConcurrent)

	// Compute and create workdir.
	workdir := filepath.Join(workdirBase, q.TaskID)
	if workdirBase == "" {
		workdir = filepath.Join(os.TempDir(), "agentwork", q.TaskID)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("mkdir workdir: %v", err))
		return
	}

	// Parse runtime args + env.
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("parse args: %v", err))
		return
	}
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(envJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, q.AgentID)

	// Build the task environment: inherit parent, layer agent env, then inject
	// agentwork-cli context so the agent can call back into the server. Runtime
	// env is layered by runtime.Launch (stdio branch), not here.
	taskEnv := os.Environ()
	for k, v := range agentEnv {
		taskEnv = append(taskEnv, k+"="+v)
	}
	selfBin, _ := os.Executable()
	binDir := filepath.Dir(selfBin)
	// agentwork-cli must sit next to the daemon binary; the agent subprocess
	// finds it via PATH. Warn once per task if missing — the agent's tool
	// calls will fail with "command not found", and this log explains why.
	cliPath := filepath.Join(binDir, "agentwork-cli")
	if _, err := os.Stat(cliPath); err != nil {
		log.Printf("daemon: agentwork-cli not found at %s; agent tool calls will fail (build it and place it next to agentwork-daemon)", cliPath)
	}
	// Build the callback URL from the listen address. net.SplitHostPort
	// handles ":port", "host:port", and "[::1]:port" uniformly; the agent
	// always calls back over loopback, so force the host to 127.0.0.1.
	addr := d.addr
	if addr == "" {
		addr = defaultListenAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("parse listen addr %q: %v", addr, err))
		return
	}
	serverURL := "http://" + net.JoinHostPort("127.0.0.1", port)
	taskEnv = append(taskEnv,
		"AGENTWORK_SERVER_URL="+serverURL,
		"AGENTWORK_TASK_ID="+q.TaskID,
		"AGENTWORK_AGENT_ID="+q.AgentID,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	// Launch the runtime connection (stdio spawn, ws dial, or tcp dial) and
	// get an ACP session over it. "定义即运行": the runtime row is the spec.
	spec := runtime.Spec{
		Transport:  transport,
		Executable: execPath,
		Args:       args,
		Endpoint:   endpoint,
		Env:        rtEnv,
	}
	sess, err := runtime.Launch(ctx, spec, taskEnv)
	if err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("launch: %v", err))
		return
	}
	defer sess.Close()

	if _, err := sess.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1}); err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("initialize: %v", err))
		return
	}

	newResp, err := sess.NewSession(ctx, acp.NewSessionRequest{Cwd: workdir})
	if err != nil {
		d.failOrRequeue(ctx, q, fmt.Sprintf("new session: %v", err))
		return
	}

	// Record the session row (history/replay; status active until the run ends).
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO session (session_id, agent_id, task_id, workdir, status, created_at)
		 VALUES (?,?,?,?,'active',?)`,
		string(newResp.SessionID), q.AgentID, q.TaskID, workdir, now()); err != nil {
		log.Printf("daemon: insert session for task %s: %v", q.TaskID, err)
	}

	// Stream events into chat_message + WS.
	h := &runHandler{daemon: d, taskID: q.TaskID, bus: d.bus, ctx: ctx}
	sess.SetEventHandler(h)

	prompt := buildPrompt(title, desc, handoff)
	if _, err := sess.Prompt(ctx, acp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: prompt}},
	}); err != nil {
		// Mark this session closed (run ended, unsuccessfully).
		d.closeSession(ctx, string(newResp.SessionID), q.AgentID)
		d.failOrRequeue(ctx, q, fmt.Sprintf("prompt: %v", err))
		return
	}

	// The handoff/wakeup note was consumed by this prompt. Clear it only after
	// the turn succeeded — if Prompt failed and failOrRequeue requeues, the
	// retry needs the note to rebuild the same prompt.
	if handoff != "" {
		if _, err := d.st.DB().ExecContext(ctx, `UPDATE task SET handoff_note='' WHERE id=?`, q.TaskID); err != nil {
			log.Printf("daemon: clear handoff_note for task %s: %v", q.TaskID, err)
		}
	}

	// Mark session closed (run ended normally) and finish the task.
	d.closeSession(ctx, string(newResp.SessionID), q.AgentID)
	d.finishTask(ctx, q, "completed", "")
}

// loadAgentEnv reads the agent-level env JSON from the DB.
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

// closeSession marks a session row closed (run ended).
func (d *Daemon) closeSession(ctx context.Context, sessionID, agentID string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE session SET status='closed' WHERE session_id=? AND agent_id=?`, sessionID, agentID); err != nil {
		log.Printf("daemon: close session %s: %v", sessionID, err)
	}
}

// failOrRequeue records a failure. If the task has been attempted fewer than
// maxAttempts times, it is requeued for retry; otherwise it is left as failed.
func (d *Daemon) failOrRequeue(ctx context.Context, q *queuedRow, summary string) {
	log.Printf("daemon: task %s failed (attempt %d): %s", q.TaskID, q.Attempt, summary)
	if q.Attempt < maxAttempts {
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE task_queue SET status='queued', attempt=attempt+1, started_at='', result_summary=? WHERE id=?`,
			summary, q.QueueID); err != nil {
			log.Printf("daemon: requeue task %s: %v", q.TaskID, err)
		}
		if _, err := d.st.DB().ExecContext(ctx, `UPDATE task SET status='queued' WHERE id=?`, q.TaskID); err != nil {
			log.Printf("daemon: set task %s queued: %v", q.TaskID, err)
		}
		d.bus.Publish(ctx, events.Event{Topic: "task:retrying", Payload: map[string]any{
			"task_id": q.TaskID, "attempt": q.Attempt + 1, "summary": summary,
		}})
		return
	}
	d.finishTask(ctx, q, "failed", summary)
}

// finishTask updates task_queue + task status and publishes a finished event.
// It will not clobber a task that transitioned to waiting_children during the
// run (the agent called wait-children mid-Prompt); the task stays waiting for
// its sub-tasks and the finished event reflects that.
func (d *Daemon) finishTask(ctx context.Context, q *queuedRow, status, summary string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE task_queue SET status=?, result_summary=?, finished_at=? WHERE id=?`,
		status, summary, now(), q.QueueID); err != nil {
		log.Printf("daemon: finish task_queue %s: %v", q.QueueID, err)
	}
	// Only flip the task status if it isn't waiting_children. If the agent
	// called wait-children during the run, the task is intentionally parked.
	res, err := d.st.DB().ExecContext(ctx,
		`UPDATE task SET status=? WHERE id=? AND status!='waiting_children'`, status, q.TaskID)
	if err != nil {
		log.Printf("daemon: finish task %s: %v", q.TaskID, err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		// Task is waiting_children — leave it. Report that in the event.
		status = "waiting_children"
	}
	d.bus.Publish(ctx, events.Event{Topic: "task:finished", Payload: map[string]any{
		"task_id": q.TaskID, "status": status, "summary": summary,
	}})

	// If this was a sub-task, maybe wake its parent. Only terminal statuses
	// (completed/failed) reach here — requeued retries don't call finishTask.
	if status == "completed" || status == "failed" {
		if err := d.taskSvc.WakeupParentIfReady(ctx, q.TaskID); err != nil {
			log.Printf("daemon: wakeup check for task %s: %v", q.TaskID, err)
		}
	}
}

// buildPrompt assembles the opening prompt for a task turn.
func buildPrompt(title, desc, handoff string) string {
	s := fmt.Sprintf("Task: %s\n\n%s", title, desc)
	if handoff != "" {
		s += "\n\nHandoff note:\n" + handoff
	}
	return s
}

// runHandler implements acp.EventHandler for one task run, streaming events
// into chat_message and the WS bus.
type runHandler struct {
	daemon *Daemon
	taskID string
	bus    *events.Bus
	ctx    context.Context // runTask's ctx, so events stop when the task/daemon stops
}

func (h *runHandler) OnAgentMessage(text string) {
	h.persist("assistant", text)
	h.bus.Publish(h.ctx, events.Event{Topic: "task:message", Payload: map[string]any{
		"task_id": h.taskID, "role": "assistant", "text": text,
	}})
}
func (h *runHandler) OnAgentThought(text string) {
	h.persist("thought", text)
	h.bus.Publish(h.ctx, events.Event{Topic: "task:thought", Payload: map[string]any{
		"task_id": h.taskID, "text": text,
	}})
}
func (h *runHandler) OnUserMessage(text string) {}
func (h *runHandler) OnToolCall(tc acp.ToolCallUpdate) {
	b, _ := json.Marshal(tc)
	// Persist tool calls so the task detail view can reconstruct the full
	// history after a refresh, not just the live WS stream. role="tool",
	// content is the serialized ToolCallUpdate.
	h.persistTool(string(b))
	h.bus.Publish(h.ctx, events.Event{Topic: "task:tool", Payload: map[string]any{
		"task_id": h.taskID, "tool": string(b),
	}})
}
func (h *runHandler) OnPlan(acp.Plan)                                 {}
func (h *runHandler) OnAvailableCommandsUpdate([]acp.AvailableCommand) {}
func (h *runHandler) OnModeUpdate(acp.SessionModeId)                   {}
func (h *runHandler) OnConfigOptionUpdate([]acp.SessionConfigOption)   {}
func (h *runHandler) OnUsageUpdate(int, int, *acp.Cost)                {}
func (h *runHandler) OnSessionInfo(string, map[string]any)             {}

func (h *runHandler) persist(role, content string) {
	if _, err := h.daemon.st.DB().ExecContext(h.ctx,
		`INSERT INTO chat_message (id, task_id, role, content, tool_calls, created_at) VALUES (?,?,?,?,'[]',?)`,
		uuid.NewString(), h.taskID, role, content, now()); err != nil {
		log.Printf("daemon: persist message for task %s: %v", h.taskID, err)
	}
}

// persistTool stores a tool call update with role="tool" and the serialized
// update in the tool_calls column (content is empty — the tool_calls JSON
// carries everything).
func (h *runHandler) persistTool(toolCallsJSON string) {
	if _, err := h.daemon.st.DB().ExecContext(h.ctx,
		`INSERT INTO chat_message (id, task_id, role, content, tool_calls, created_at) VALUES (?,?,'tool','',?,?)`,
		uuid.NewString(), h.taskID, toolCallsJSON, now()); err != nil {
		log.Printf("daemon: persist tool call for task %s: %v", h.taskID, err)
	}
}

// ── helpers ──

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
