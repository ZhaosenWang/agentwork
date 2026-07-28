# agentwork — Design

> Task-driven multi-runtime agent management platform. Treats CLI agents
> (openagent-cli, opencode, codex, …) as schedulable runtimes and unifies
> task creation, assignment, execution, and coordination (handoff / sub-task).

---

## 1. Concepts

| Concept | What it is | Lives in |
|---|---|---|
| **runtime** | Launch spec — how to connect to an ACP-speaking program. `transport` (stdio/ws/tcp) + `executable`+`args` (stdio) or `endpoint` (ws/tcp) + `env` + `protocol`. Pure config, no capabilities. | `runtime` table |
| **agent** | A runtime + a persona (system_prompt / model / workdir / env / max_concurrent). The unit of concurrency: each agent has a worker with a semaphore. | `agent` table |
| **task** | A unit of work, assigned to an agent or human. Has a state machine and optional `parent_id` for sub-task coordination. | `task` table |
| **task_queue** | Dispatch buffer. A row here means "this task should run on this agent". daemon claims rows and runs them. | `task_queue` table |

### Runtime = launch spec, agent = runtime + persona

A **runtime** row is the complete launch spec ("define to run"): the daemon reads it
and connects — no extra configuration. Three transports:

- `stdio` — spawn `executable` + `args` as a subprocess, talk ACP over stdin/stdout.
- `ws` — dial a WebSocket `endpoint`, talk ACP over text frames.
- `tcp` — dial a TCP `endpoint`, talk ACP over newline-delimited JSON.

An **agent** picks one runtime (`runtime_id` FK) and adds persona: system prompt,
model override, workdir base, per-agent env, concurrency limit.

### Per-task connection model

Each task run opens a **fresh** connection, runs one ACP `Prompt`, drains
events, and tears the connection down. "Wake up the parent" = re-enqueue the
parent task with a wakeup prompt, not append to a live session. The long-lived
session model (ACP `session/load` / `session/resume` reuse across multiple
prompts on one agent) is a future direction; the `session` table and
`agent.status`/`agent.pid` columns are retained for it.

---

## 2. Architecture

```
               HTTP API (internal/server)
               ┌──────────────────────────────────────┐
  frontend ──► │ /runtimes /agents /tasks /schedules   │
  CLI ───────► │ /tasks/{id}/assign|cancel|wait|messages│
               │ /ws (WebSocket fan-out) /healthz       │
               └──────────────────┬───────────────────┘
                                  │ handler.Handlers
               ┌──────────────────▼───────────────────┐
               │          service layer                │
               │  Task   Agent   Runtime   Schedule    │  ← CRUD + business rules
               └──────────────────┬───────────────────┘
                                  │ writes DB + publishes events
               ┌──────────────────▼───────────────────┐
               │          store (SQLite WAL)            │
               │  task task_queue agent runtime         │
               │  session chat_message activity_log     │
               │  schedule schedule_run                 │
               └──────────────────────────────────────┘
                                  │
               ┌──────────────────▼───────────────────┐
               │          daemon (scheduler)            │
               │  • polls task_queue → claims → runs    │
               │  • polls schedule → fires → creates    │
               │    task + enqueues                    │
               │  • runtime.Launch (stdio/ws/tcp) →     │
               │    acp.Session → Initialize → Prompt   │
               │  • recovers stuck 'running' on restart │
               └──────────────────────────────────────┘
                                  │
               ┌──────────────────▼───────────────────┐
               │     events.Bus (in-process pub/sub)    │
               │  task:* agent:* schedule:*             │ → WS hub → frontend
               └──────────────────────────────────────┘
```

### Layers

- **store** — SQLite (modernc.org/sqlite, pure Go, no cgo). Single file, WAL mode.
  DSN pragmas propagate `foreign_keys=ON`, `journal_mode=WAL`,
  `busy_timeout=5000` to every pooled connection. Schema applied on open via
  `CREATE TABLE IF NOT EXISTS`; no migration tool.
- **service** — business logic. Each service (Task/Agent/Runtime/Schedule) owns
  its CRUD + validation + event publishing. `TaskService` also owns sub-task
  coordination (`WaitChildren`, `WakeupParentIfReady`).
- **daemon** — scheduler. Owns per-agent workers, the task_queue dispatch loop,
  schedule firing, and `runTask` (launch runtime → ACP handshake → Prompt →
  drain events → record outcome). Subscribes to `agent:created`/`agent:deleted`
  for worker lifecycle.
- **server** — HTTP + WebSocket boundary. Mounts CRUD routes + WS hub. Services
  are wired here; daemon is coupled only via the shared `events.Bus`.
- **events.Bus** — in-process pub/sub. Handlers run in goroutines with panic
  recovery. No persistence, no cross-node relay. Used to notify the WS hub
  (frontend) and daemon (worker lifecycle).
- **runtime** — transport layer. `runtime.Launch(ctx, Spec, taskEnv)` reads a
  runtime spec, opens a connection (stdio spawn / ws dial / tcp dial), and
  returns an `*acp.Session`. "定义即运行".
- **acp** — ACP v1 protocol layer (JSON-RPC 2.0). `acp.Session` is
  transport-agnostic: it speaks the protocol over any `io.Reader` + `io.Writer`
  pair. `ConnectStdio` is a convenience factory for the stdio transport.

### Binaries

- `agentwork-daemon` — HTTP server + embedded daemon. Single process.
- `agentwork-cli` — agent-side tool. The daemon injects it into the agent
  subprocess's `PATH` plus `AGENTWORK_SERVER_URL` / `AGENTWORK_TASK_ID` /
  `AGENTWORK_AGENT_ID` env vars, so the agent can call back to produce
  structured side effects (handoff, create sub-task, list tasks, append
  messages, wait-children). CLI-as-tool.

---

## 3. Task lifecycle

```
backlog ──assign──► queued ──claim──► running ──┬──► completed
                   │                  │          │
                   │                  ├──► failed (attempts exhausted)
                   │                  │
                   │                  ├──► retrying ──► queued (attempt+1)
                   │                  │
                   │                  └──► waiting_children ──children done──► queued
                   │
                   └──cancel──► cancelled
```

- **backlog** — parking lot. Assigned but not queued; doesn't run.
- **queued** — in `task_queue`, waiting for the daemon to claim.
- **running** — daemon claimed it, ACP session active.
- **waiting_children** — agent called `wait-children`; parent is suspended until
  all non-terminal children finish.
- **completed / failed / cancelled** — terminal.

### Dispatch chain

```
task assigned to agent + status≠backlog
  → enqueueInTx: INSERT task_queue (status=queued) + UPDATE task status=queued
  → daemon polls task_queue (claimQueued: atomic UPDATE…RETURNING)
  → routes to per-agent worker (semaphore-bounded concurrency)
  → runTask: runtime.Launch → acp Initialize → NewSession → Prompt
  → drain EventHandler (OnAgentMessage/OnToolCall/…) → persist chat_message + publish events
  → finishTask: UPDATE task_queue + task status, clear handoff_note, trigger WakeupParentIfReady
```

### Sub-task coordination

1. Parent agent calls `agentwork-cli task create --parent <parent_id>` to spawn children.
2. Parent agent calls `agentwork-cli task wait-children` → parent status = `waiting_children`.
3. Each child runs independently through the normal dispatch chain.
4. When a child reaches a terminal status, `WakeupParentIfReady` checks: is the
   parent `waiting_children`? Are all children terminal? (checked inside one
   transaction to prevent races). If yes → re-enqueue parent with a wakeup
   prompt summarizing the children.
5. Parent runs a second time, sees the wakeup prompt, continues.

The wait set is **dynamic**: children created after `wait-children` still count.
Wakeup checks "all non-terminal children" at the moment of the last child's
finish, not a snapshot from when `wait-children` was called.

### Handoff

Same task transferred from agent A to agent B:
- A's session is frozen (`status='frozen'`), history retained.
- B opens a fresh session; its opening prompt = `handoff_note` + A's
  `chat_message` history.
- Implemented in `TaskService.Assign`.

---

## 4. Schedule (cron)

A schedule is a cron-triggered task template. The daemon scans for due
schedules and fires each in a transaction:

```
INSERT task (status=queued, created_by=system)
INSERT task_queue (status=queued)
INSERT schedule_run (planned_at)  ← unique index uq_schedule_run_planned enforces idempotency
```

If a concurrent tick already fired the same `(schedule_id, planned_at)`, the
unique index rejects the insert, the tx rolls back (no orphan task), and
`next_run_at` advances. This is the "external event → auto-create task →
enqueue" pattern — webhook/issue ingestion follows the same shape.

---

## 5. Data model

```sql
runtime(id, name, transport, executable, args, endpoint, env, protocol, created_at)
  -- transport: stdio|ws|tcp
  -- stdio uses executable+args; ws/tcp uses endpoint

agent(id, name, description, runtime_id, system_prompt, model, workdir_base,
      env, max_concurrent, status, pid, created_at)
  -- status: offline|online|crashed

task(id, title, description, parent_id, assignee_type, assignee_id,
     status, handoff_note, created_by_type, created_by_id, created_at)
  -- status: backlog|queued|running|waiting_children|completed|failed|cancelled
  -- assignee_type: agent|human

task_queue(id, task_id, agent_id, status, attempt, result_summary,
           queued_at, started_at, finished_at)
  -- status: queued|running|completed|failed

session(session_id, agent_id, task_id, workdir, status, created_at,
        PRIMARY KEY (session_id, agent_id))
  -- session_id is ACP server-generated; unique only within one agent

chat_message(id, task_id, role, content, tool_calls, created_at)
activity_log(id, task_id, actor_type, actor_id, action, detail, created_at)
schedule(id, name, title_template, description, assignee_id, cron_expression,
         timezone, enabled, next_run_at, last_run_at, created_at)
schedule_run(id, schedule_id, task_id, planned_at, status, created_at)
  -- UNIQUE(schedule_id, planned_at) for idempotent firing
```

---

## 6. Transport / protocol separation

```
runtime.Launch(spec)           ← transport layer: stdio spawn / ws dial / tcp dial
  │
  ▼
acp.NewSession(r, w, closeFn)  ← protocol layer: JSON-RPC 2.0 over io.Reader/Writer
  │
  ▼
sess.Initialize / NewSession / Prompt / …  ← ACP v1 methods, transport-agnostic
```

`acp.Session` holds only `io.Reader` + `io.Writer` + a close function — no
process handle, no socket. The transport layer (`internal/runtime`) is
responsible for building the read/write pair and the cleanup closure. Adding a
new transport is one `case` in `runtime.Launch`; the protocol layer is
untouched.

---

## 7. Extension points

- **New HTTP route** — one line in `handler.Mount` + one method. Services are
  wired in `server.ListenAndServe`.
- **New transport** — one `case` in `runtime.Launch`.
- **New event type** — `bus.Publish` + add the topic to `ws/hub.go` topics list.
- **External trigger → task** (webhook, issue poll, …) — call
  `TaskService.Create` with `Status=queued` + agent assignee. Reuses
  `enqueueInTx` and the entire dispatch chain. Schedule is the existing
  precedent. Idempotency requires a unique constraint on an external event id
  (schedule uses `uq_schedule_run_planned`; webhook needs its own).
- **Non-ACP protocol** — the `runtime.protocol` column is reserved; today all
  transports speak ACP. Branching would go in `runtime.Launch`.

### Not extension points (would require structural change)

- **Multi-node** — `events.Bus` is in-process, no cross-node relay. Single
  daemon + single DB. Multi-node needs an external message bus.
- **Push-based dispatch** — daemon polls `task_queue`. For lower latency, an
  in-process channel or SQLite notify mechanism could replace the poll; the
  dispatch logic (`claimQueued`) stays the same.

---

## 8. Tech stack

- **Go** + `net/http` ServeMux (Go 1.22+ method+path patterns) + hand-written SQL
- **SQLite** via `modernc.org/sqlite` (pure Go, no cgo), WAL mode
- **ACP v1** client SDK in `internal/acp/` (vendored copy from openagent-go;
  sync manually if upstream changes)
- **gorilla/websocket** for the WS hub and the ws transport
- **robfig/cron/v3** for schedule cron expressions
- **Vue 3 + Element Plus** frontend
- Single-user, no auth — open-source for managing your own agents
