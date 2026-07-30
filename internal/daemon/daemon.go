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
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eushing/agentwork/internal/events"
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

	mu      sync.Mutex
	workers map[string]*agentWorker // agentID → per-agent scheduler
	stopped bool
	ctx     context.Context
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

func New(st *store.Store, bus *events.Bus, addr string, protoReg *proto.Registry, goalSvc *service.GoalService, runSvc *service.RunService, squadSvc *service.SquadService, schedSvc *service.ScheduleService) *Daemon {
	d := &Daemon{
		st: st, bus: bus, addr: addr,
		protoReg: protoReg, goalSvc: goalSvc, runSvc: runSvc,
		squadSvc: squadSvc, schedSvc: schedSvc,
		workers: make(map[string]*agentWorker),
	}
	bus.Subscribe("agent:created", d.onAgentCreated)
	bus.Subscribe("agent:deleted", d.onAgentDeleted)
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

// ── run execution ──

// runTask opens a transport, hands it to the protocol backend for one Prompt,
// drains events into chat_message + WS, and finishes the run (which triggers
// goal-layer reconciliation).
func (d *Daemon) runTask(ctx context.Context, q *service.ClaimedRow) {
	var title, desc, handoff, workdirBase, systemPrompt, transport, provider, execPath, argsJSON, endpoint, rtEnvJSON string
	var maxConcurrent int
	var isLeaderRun bool
	var squadID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT g.title, g.description, g.handoff_note, a.workdir_base, a.system_prompt,
		        r.transport, r.provider, r.executable, r.args, r.endpoint, r.env, a.max_concurrent,
		        r2.is_leader_run, r2.squad_id
		 FROM run r2
		 JOIN goal g ON g.id = r2.goal_id
		 JOIN agent a ON a.id = r2.agent_id
		 JOIN runtime r ON r.id = a.runtime_id
		 WHERE r2.id = ?`, q.RunID).
		Scan(&title, &desc, &handoff, &workdirBase, &systemPrompt, &transport, &provider, &execPath, &argsJSON, &endpoint, &rtEnvJSON, &maxConcurrent, &isLeaderRun, &squadID)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("load config: %v", err))
		return
	}

	d.ensureWorker(q.AgentID, maxConcurrent)

	// Per-run working directory.
	runRowWorkdir := filepath.Join(workdirBase, q.RunID)
	if workdirBase == "" {
		runRowWorkdir = filepath.Join(os.TempDir(), "agentwork", q.RunID)
	}
	if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
		d.failRun(ctx, q, fmt.Sprintf("mkdir workdir: %v", err))
		return
	}

	// Inject the agent's identity + team roster / squad briefing into the
	// workdir so the agent subprocess discovers who it is and who it can hand
	// off to (AGENTS.md).
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
	// The idle watchdog cancels promptCtx to interrupt a hung turn; backend
	// Execute must run under promptCtx (not the bare ctx) so the cancel takes.
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var inFlightTools atomic.Int32
	promptCtx, promptCancel := context.WithCancel(ctx)
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
	switch result.Status {
	case proto.StatusCompleted:
		// The handoff/wakeup note is consumed by the goal layer (ReconcileOnRunEnd
		// clears it only after confirming this run owns the goal and the goal
		// promotes to done). The daemon must NOT clear it here: on a handoff this
		// run no longer owns the goal, and clearing would wipe the new owner's
		// note (see P2 in the bug review).
		_ = d.runSvc.MarkSession(ctx, q.RunID, result.SessionID, runRowWorkdir)
		d.finishRunOK(ctx, q, result.Output)
	case proto.StatusCancelled:
		d.finishRun(ctx, q, "cancelled", "idle watchdog: "+result.Output)
	case proto.StatusFailed, proto.StatusAborted:
		d.finishRun(ctx, q, "failed", result.Output)
	}
}

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

func (d *Daemon) persistEvent(ctx context.Context, runID string, ev proto.Event) {
	role := "assistant"
	content := ev.Text
	toolCalls := "[]"
	if ev.Type == proto.EventThought {
		role = "thought"
	} else if ev.Type == proto.EventToolUse || ev.Type == proto.EventToolResult {
		role = "tool"
		content = ""
		tc, _ := json.Marshal(ev)
		toolCalls = string(tc)
	}
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
	if handoff != "" {
		// A handoff/wakeup note scopes THIS turn. It is placed AHEAD of the
		// original description, which is now *context* (not a fresh to-do
		// list). This prevents the sub-goal loop: a woken parent that sees the
		// child-summary note must NOT blindly re-execute the original
		// description's "create a sub-task" steps. If the note reports the work
		// already complete, the agent ends its turn rather than fanning out
		// again.
		return "Task: " + title + "\n\n" +
			"Context (what this goal is about; do NOT blindly redo these steps):\n" + body + "\n\n" +
			"Scope for THIS run (follow the note; do not redo steps it describes as done):\n> " + handoff + "\n\n" +
			"If the note reports the work is already complete, do NOT start new work — end your turn immediately.\n"
	}
	return fmt.Sprintf("Task: %s\n\n%s", title, body)
}

// buildAgentGuide writes the "## Team & Coordination" block that every run's
// AGENTS.md gets: the roster of teammates plus the full agentwork-cli
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
// leader runs) squad briefing into {workdir}/AGENTS.md so the subprocess
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
	return os.WriteFile(filepath.Join(workdir, "AGENTS.md"), []byte(b.String()), 0o644)
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
		`SELECT id, title_template, description, assignee_type, assignee_id, cron_expression, timezone, next_run_at
		 FROM schedule WHERE enabled=1 AND next_run_at != '' AND next_run_at <= ?`, nowStr)
	if err != nil {
		log.Printf("daemon: schedule query: %v", err)
		return
	}
	var due []scheduleDueRow
	for rows.Next() {
		var r scheduleDueRow
		if err := rows.Scan(&r.ScheduleID, &r.TitleTemplate, &r.Description, &r.AssigneeType, &r.AssigneeID, &r.CronExpression, &r.Timezone, &r.NextRunAt); err != nil {
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
	ScheduleID, TitleTemplate, Description, AssigneeType, AssigneeID, CronExpression, Timezone, NextRunAt string
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,'active',?,'','','system',?,?)`,
		goalID, r.TitleTemplate, r.Description, r.AssigneeType, r.AssigneeID, r.ScheduleID, ts); err != nil {
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