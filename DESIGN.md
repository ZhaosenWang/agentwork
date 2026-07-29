# agentwork — Design

> Task-driven multi-protocol agent management platform. Treats CLI agents
> (claude / codex / opencode / openagent-cli, …) as schedulable runtimes and
> unifies goal creation, assignment, execution, and coordination (handoff /
> sub-goal / @mention / squad).
>
> Single process, single machine, single user, no auth. Local SQLite for
> persistence, in-process event bus.
>
> This design is set from scratch for agentwork — it does not inherit the
> historical baggage of the reference implementation. See `DESIGN.zh.md` for
> the canonical Chinese version; this English document tracks it.

---

## 1. One-line mental model

> **A goal is a work item (who owns it, how far it's progressed); a run is one
> execution (some agent's turn on it). The same goal may be executed many times
> and handed off across agents, with full history retained. A run has no
> authority — status is arbitrated by the goal layer.**

Agents do not talk to each other directly. All coordination is asynchronous,
goal-mediated, queue-driven: agents read and write the same goal (comments /
handoff note / status / shared workdir). Agents produce **structured side
effects** by calling `agentwork-cli` — the system never parses agent output to
infer intent.

---

## 2. Core concepts

| Concept | What it is | Lives in |
|---|---|---|
| **runtime** | A launch spec — how to connect to a protocol-speaking program. `transport` (stdio/ws/tcp) + `executable`+`args` (stdio) or `endpoint` (ws/tcp) + `env`. Pure config, no capabilities. | `runtime` table |
| **provider** | A protocol kind — which wire protocol the agent speaks, deciding which backend is attached. `acp` / `jsonl` / `jsonrpc`. A field on the runtime. | `runtime.provider` |
| **agent** | A runtime + a persona (system_prompt / model / workdir / env / max_concurrent). The concurrency unit: each agent has a worker + a semaphore. | `agent` table |
| **squad** | A routing group that does no work itself. Has a leader (an agent). Assigning to a squad / @mentioning a squad routes only to the leader, who delegates sub-goals. | `squad` table |
| **goal** | A work item (product plane). Assignable to an agent / squad / human. Has a state machine and optional `parent_id` for sub-goal coordination. **The sole holder of state authority.** | `goal` table |
| **run** | One execution (execution plane): one agent's turn on one goal. The `run` table is the execution projection of a goal. **No authority — on terminal status it reports up to the goal layer.** | `run` table |

### What the two-layer model really is

The point is not "splitting a task into two layers" as a mechanical act — it's
**where state authority lives**:

- **The run layer has no authority.** A run is just the execution record of
  "some agent took a turn on this goal." On reaching a terminal status
  (completed/failed/cancelled) it does NOT write goal status directly — it
  reports "I finished, here's the result" to the goal layer.
- **The goal layer is the sole authority.** The goal state machine alone
  decides "is this work done / should I open another turn / hand it to whom."
  Any write affecting the goal's progression goes through one arbitration
  method, `reconcileOnRunEnd(run)`, which checks the current state before
  ruling.

### Why this layering

agentwork's from-scratch design internalizes the authority into the goal state
machine: a run's completion is arbitrated by the goal layer, which checks
"does the reporting run still belong to the current owner?" and discards if
not. It is self-consistent without an external authority — so it never needs
to agonize over "abort the old run or let it run parallel": **whether the old
run is aborted or not does not affect correctness**, because its result isn't
entitled to change goal status.

---

## 3. Architecture

```
┌──────────────────────────────────────────────────────────┐
│  agentwork-daemon (single process)                        │
│                                                           │
│  HTTP API + WS hub  ──→  service layer  ──→  store(SQLite) │
│        │                       │                           │
│        │                       │ bus.Publish (after commit)│
│        ▼                       ▼                           │
│      WS fan-out            daemon scheduler                │
│  (frontend / cli)          claim run → runTask             │
│                                  │  by runtime.provider     │
│                                  ▼                          │
│                           runtime.Open(spec)               │
│                                  │  returns transport R/W    │
│                                  ▼                          │
│                           backend.Execute(Session)         │
│                                  │  acp|jsonl|jsonrpc        │
│                                  ▼                          │
│                           agent CLI subprocess             │
│                                  │ calls agentwork-cli      │
│                                  ▼  (+ env: SERVER_URL…)    │
└──────────────────────────────────┼─────────────────────-─-─┘
                                   ▼
                          agentwork-cli (agent-side tool, CLI-as-tool)
```

### Layer responsibilities

| Layer | Package | Responsible for | Not |
|---|---|---|---|
| **store** | `internal/store` | SQLite + schema, hand-written SQL | business logic |
| **service** | `internal/service` | business rules + event publish (Goal/Run/Agent/Squad/Schedule) + **goal-layer arbitration `reconcileOnRunEnd`** | executing agents, calling LLMs |
| **daemon** | `internal/daemon` | run dispatch/scheduling, runTask, idle watchdog; **on run terminal, calls goal-layer arbitration, never writes status directly** | business decisions, state authority |
| **runtime** | `internal/runtime` | transport layer: opens a connection per the runtime spec, returns a transport Conn for the backend | speaks no protocol |
| **proto** | `internal/proto` | protocol layer: multiple backends (acp / jsonl / jsonrpc), a unified `Backend.Execute → {Events, Result}` abstraction | speaks no transport |
| **server** | `internal/server` | HTTP + WS boundary, CORS | |
| **events** | `internal/events` | in-process pub/sub (synchronous, panic recovery) | no persistence, no cross-node |
| **cli** | `cmd/agentwork-cli` | agent-side callback tool | |
| - | `cmd/agentwork-daemon` | main: wires and starts HTTP + daemon | |

### Binaries

- `agentwork-daemon` — HTTP server + embedded daemon. Single process.
- `agentwork-cli` — agent-side tool. The daemon injects it into the agent
  subprocess's PATH plus `AGENTWORK_SERVER_URL` / `AGENTWORK_GOAL_ID` (product
  plane id) / `AGENTWORK_RUN_ID` (execution plane id) / `AGENTWORK_AGENT_ID`,
  so the agent can call back to produce structured side effects.

---

## 4. Data model

### Two primary state tables (the two-layer mediation)

```sql
-- goal: a work item (product plane). The sole holder of state authority.
goal (
  id, title, description,
  parent_id            → goal.id,        -- sub-goal
  assignee_type        TEXT,             -- agent | squad | human
  assignee_id          TEXT,             -- agent.id | squad.id | ''(human)
  status               TEXT,             -- backlog|active|done|failed|blocked|cancelled
  handoff_note         TEXT,
  created_by_type      TEXT,             -- human | agent
  created_by_id        TEXT,
  created_at           TEXT
)
  -- backlog is a semantic invariant: assigning into backlog does NOT start a run.
  -- parent_id implements a sub-goal (not a sub-run).

-- run: one execution (execution plane). No authority — on terminal status it
-- only reports up to the goal layer for authoritative state change.
run (
  id,
  goal_id             → goal.id,
  agent_id            → agent.id,
  session_id          TEXT,            -- protocol-returned; for history/future resume
  workdir             TEXT,
  status              TEXT,            -- queued | running | completed | failed | cancelled
  attempt             INT,
  result_summary      TEXT,
  trigger_comment_id  → comment.id,    -- which comment caused this run (mention/child-done)
  is_leader_run       INT,             -- 1 if a squad leader run
  squad_id            → squad.id,      -- the squad a leader run belongs to
  queued_at, started_at, finished_at, created_at
)
```

**Two state machines, decoupled:**
```
goal.status (product — where is this work?)   run.status (execution — is the agent running?)
backlog → active → done | failed              queued → running → completed | failed
        ↘ blocked                             ↘ cancelled
        ↘ cancelled
```

`blocked` means the goal is waiting on its sub-goals to finish.

**The backlog semantic invariant:** assigning an agent into `backlog` does NOT
start a run; only `backlog → active` (or creating/assigning while active)
enqueues a run.

### Other tables

```sql
runtime (id, name, provider, executable, args, endpoint, transport, env, created_at)
  -- provider ∈ acp|jsonl|jsonrpc: which backend to attach

agent (id, name, description, runtime_id, system_prompt, model, workdir_base,
       env, max_concurrent, created_at)
  -- No status/pid dead columns: meaningless under the per-task model; they
  -- belong to the future long-lived-session model.

squad (id, name, description, leader_id → agent.id, instructions, created_at)
  -- A routing group that does no work; everything routes to the leader. The
  -- leader must be an agent; squads cannot nest.

squad_member (id, squad_id → squad.id, member_type ∈ {agent, human}, member_id, role, created_at,
              UNIQUE(squad_id, member_type, member_id))
  -- Polymorphic members. No nesting ('squad' not allowed as a member type).

comment (id, goal_id, author_type ∈ {human, agent, system}, author_id,
         parent_id → comment.id,   -- one level of threading (arbitrary depth may come later)
         content TEXT,              -- Markdown, may carry structured mention URIs
         created_at)

chat_message (id, run_id → run.id, role, content, tool_calls, created_at)
  -- The run's output stream cache (for the run detail view); keyed by run, not goal.

activity_log (id, goal_id, actor_type, actor_id, action, detail, created_at)
  -- audit: handoff / child_created / children_done / squad_leader_evaluated / cancelled …

schedule (id, name, title_template, description, assignee_type, assignee_id,
          cron_expression, timezone, enabled, next_run_at, last_run_at, created_at)
  -- cron-triggered: each fire clones a fresh goal + run, idempotent via the
  -- schedule_run unique index.
schedule_run (id, schedule_id, goal_id, planned_at, status, created_at,
             UNIQUE(schedule_id, planned_at))
```

A `session` table is intentionally NOT added (the per-task model opens a fresh
connection each run; a long-lived model would require it — deferred).

---

## 5. Coordination primitives

Four "hand off / advance" mechanisms. All asynchronous, goal-mediated,
queue-driven.

### 5.1 Reassignment (assignee handoff)

Changing `goal.assignee` A→B (agentwork-cli `goal assign`):
1. The goal layer UPDATEs assignee + appends activity_log(handoff) + sets
   handoff_note (if any).
2. Enqueues a run on B. **Does NOT cancel A's in-flight run.**
3. When A's run finishes it goes through goal-layer arbitration: the goal
   layer sees `run.agent != goal.assignee` → **discards the result (does not
   touch goal status)**, keeping it as history.

Whether the old run is aborted is irrelevant to correctness: **changing the
goal's owner is a goal-layer operation; an in-flight run is the previous
owner's tail, and its result isn't entitled to change goal status.**

### 5.2 Sub-goal wait (fan-out + wait-children)

1. The parent run's agent calls `goal create --parent=<parent>` to create
   several sub-goals.
2. The parent run's agent calls `goal wait` → the parent goal enters `blocked`
   (the wait set).
3. The parent run ends (this turn is done, because the parent is parked).
4. Each sub-goal runs its own normal dispatch chain (assigned → enqueued → run).
5. On reaching a terminal status, a sub-goal triggers a **wake check**: is the
   parent `blocked`? are all non-terminal sub-goals now terminal? (queried in
   one transaction, race-free). If yes → enqueue a new run on the parent's
   current assignee with a handoff_note summarizing the children.
6. The parent's second run sees the summary and continues.

The **wait set is dynamic**: sub-goals created after `wait` was called also
count. The wake checks "all currently non-terminal sub-goals," not a snapshot
from when wait was called. The wake is idempotent via the "at most one pending
run per (goal,agent)" coalesce (so two children finishing in parallel don't
double-wake the parent).

### 5.3 @mention comment trigger

An agent comments on a goal and `@mentions another agent`, triggering a new
execution:
1. The agent calls `goal comment <goal> --content "[@Codex](mention://agent/<uuid>) ..."`.
2. After the comment is persisted, the server parses the comment body (**only
   the persisted body, never agent stdout**).
3. `mention://agent/<uuid>` → enqueue a new run on the **mentioned agent**
   (**same goal_id, different agent_id**), does NOT cancel the in-flight run.
   `trigger_comment_id` records provenance.
4. `mention://squad/<uuid>` → routes to the squad's **leader** (a leader run).
5. `mention://human/<uuid>` → just renders a link, no run (humans don't execute).
6. `@all` (`mention://all/all`) → **suppresses auto-trigger** (enqueues no run),
   notifies humans only.

Mentions are structured Markdown URIs (`[@Name](mention://agent/<uuid>)`),
**uuid-only — `@handle` prose does NOT match**. The agent must first
`agentwork-cli agent list` to resolve the UUID, then write the structured link.
This is the concrete landing of "agents produce structured side effects via
CLI."

### 5.4 Squad routing group

Compose several agents into a team, route to the leader, who delegates
sub-goals by capability:
- Assigning a goal to a squad / @mentioning a squad → enqueue a run only on
  the **leader** (a leader run); no member fan-out. Avoids waking N members
  at once.
- The leader run's opening prompt gets the **squad briefing** = Operating
  Protocol + Roster + Instructions.
  - A roster line: `- <name> — <kind>[, role: "<role>"][ — skills: a, b] —
    [@Name](mention://agent/<uuid>)` — the leader delegates by capability,
    not by guessing role labels.
- "Delegation" = the 5.1/5.2 mechanisms: the leader assigns a sub-goal to a
  member agent, recursively.
- The leader records its evaluation with `agentwork-cli squad activity <goal>
  action|no_action|failed --reason "..."` (into activity_log, as timeline).

**Status authority is gated separately**:
briefing injection keys on `is_leader_run`; but whether the leader may change
the parent goal's status additionally requires
`goal.assignee_type=="squad" && assignee_id==squad.id`. A squad merely
@mentioned into **someone else's goal** is a guest — its briefing carries the
"do NOT change this goal's status" clause; only the leader that owns the goal
has the authority to push it to `done`/`active`.

**Child-done wake:** when a sub-goal finishes, wake the parent goal's current
assignee — parent assigned to an agent → wake that agent; parent assigned to a
squad → wake its leader (enqueue a leader run). No invocation gate (the leader
already owns the parent — permission was enforced at assignment; the child's
completer has no resolvable human originator, so re-gating would wrongly deny a
default-private leader). Bounded by the "at most one pending run per
(goal,agent)" coalesce.

---

## 6. The protocol layer (multi-protocol)

Lifts the existing `internal/acp` into a backend abstraction; ACP is one of
several.

```go
package proto

// Backend is one protocol implementation. Execute starts its protocol on the
// transport, streams Events, delivers one Result, and closes both channels.
type Backend interface {
    Execute(ctx context.Context, spec ExecuteSpec) (*Run, error)
}

// Conn is an already-open transport connection, produced by runtime.Open.
type Conn struct {
    R      io.Reader
    W      io.Writer
    Close  func() error
    Stderr io.Reader // nil for non-stdio transports
}

type ExecuteSpec struct {
    Conn   Conn    // the transport connection built by runtime.Open
    Cwd    string  // working directory for the run
    Prompt string  // the pre-built opening prompt
}

type Run struct {
    Events <-chan Event   // streamed events (Message/Thought/ToolUse/ToolResult/…)
    Result <-chan Result  // exactly one terminal outcome, then closed
}

type Result struct {
    Status    Status  // completed | failed | aborted | cancelled
    Output    string
    SessionID string  // for the future long-lived model to resume
}
```

Three backends:
- **`proto/acp`** — wraps the existing `internal/acp` (ACP v1 over JSON-RPC).
  Implemented first; the only one wired end-to-end today.
- **`proto/jsonl`** — claude/opencode-style `--output-format json`
  single-direction JSONL. Turn completion inferred from `step_start`/
  `step_finish` structural pairing; if EOF lands mid-open-step, **fail-closed**
  ("stream ended without a terminal signal"), never misjudged as completion.
- **`proto/jsonrpc`** — codex `app-server`-style bidirectional JSON-RPC 2.0,
  with an explicit `turn/completed` notification and completedTurnIDs dedup.

The runtime's `provider` field selects the backend. Adding a protocol = adding a
backend; no change to the daemon or scheduler.

---

## 7. Daemon scheduling

### Per-agent worker + semaphore

Each agent has a worker whose semaphore capacity is `max_concurrent`. One
agent's runs parallelize up to its limit; different agents are independent.

### Claim improvement (fixes head-of-line blocking)

The old global `LIMIT 1` claim let one saturated agent stall all other agents'
goals. Now **claim does not grab one global row; it claims per-agent**: the
daemon maintains each worker's free slots, and claims queued runs only within
the set of agents that currently have free capacity. A saturated agent no
longer blocks other agents' dispatch.

### Idle watchdog

If pure silence exceeds `window` (default, short) the turn is cancelled; **while
a tool is in flight the larger `idleToolWindow` applies** — an `inFlightTools`
atomic counter is bumped on `tool_use` with no matching `tool_result`. As long
as the agent keeps emitting, it is never killed — only silence is bounded
(MUL-3064 principle).

### Run terminal → goal-layer arbitration

When a run finishes (Result arrives), the daemon does **not** directly SQL
UPDATE the goal status — it calls `service.RunService.Finish`, which stamps the
run row then delegates the outcome to `GoalService.ReconcileOnRunEnd(run)`:

```
reconcileOnRunEnd(run):
  read goal current status + assignee
  if run.agent != current assignee:        # reassigned / handed off
      keep as history, don't touch goal status, return  # A's tail can't pollute B's goal
  if goal.status == cancelled:             # was cancelled mid-run
      keep as history, don't touch, return
  switch run.status:
    completed:
      if non-terminal sub-goals remain: leave (the child-done flow owns the wake)
      else: goal → done, and trigger wake-parent
    failed:
      if attempts remain: enqueue a new run (attempt+1)
      else: goal → failed
  emit goal:finished
```

**This is where agentwork's state authority is internalized.** The prior draft's
direct-SQL `UPDATE task SET status=…` was the root cause of the handoff/cancel
bugs; the from-scratch design replaces it with goal-layer arbitration, fixing
them at the design level rather than patching.

---

## 8. Extension points

- **New HTTP route** — add a line in `handler.Mount` + a service method. Wired
  in `server.ListenAndServe`.
- **New transport** — add a case in `runtime.Open`.
- **New protocol backend** — add a backend implementation in `internal/proto`;
  add a `provider` value to the runtime table.
- **New event type** — `bus.Publish` + add the topic to the `ws/hub.go`
  allowlist.
- **External trigger → goal** (webhook, issue polling) — call
  `GoalService.Create`(status=active) + assign, reusing the same dispatch
  chain. Schedule is the existing precedent. Idempotency via a unique index on
  the external event id.
- **Long-lived agent model** (future, architectural) — launch a long-lived
  process on agent creation; reuse `session_id` per run (ACP `session/load`/
  `resume`). At that point the `session` table and agent liveness columns are
  enabled. Today the per-task model opens a fresh connection each run.

### Not extension points (require structural change)

- **Multi-node** — `events.Bus` is in-process, no cross-node relay; a single
  daemon + single DB. Multi-node needs an external message bus.
- **Multi-user / auth** — out of scope; single user.

---

## 9. Engineering invariants (hard rules keeping the system correct)

1. **Runs have no authority; the goal layer is the sole authority.** Any write
   affecting goal status goes through `ReconcileOnRunEnd`; a run directly SQL-
   changing goal status is a bug.
2. **State machines are decoupled.** The execution/product state machines
   don't tug at each other; reassignment, cancel, and child-wait only touch
   the goal.
3. **No parsing agent stdout for intent.** Agents emit structured side effects
   via CLI; mentions are structured Markdown URIs (uuid-only), parsed only
   from persisted comment bodies.
4. **The backlog semantic invariant.** Assigning into backlog starts no run.
5. **At most one pending run per (goal, agent).** Coalescing safety net (a
   service-layer check — a partial unique index would wrongly block retry runs
   that legitimately coexist). Prevents duplicate handoff/mention/child-done
   enqueues.
6. **Squads do no work.** Routing only reaches the leader, no member fan-out;
   status authority is gated separately (only the owning leader may change
   status).
7. **Per-run workdir isolation.** Each run gets `{workdir_base}/{run_id}`
   (or reused by (agent,goal) once the long-lived model lands).

---

## 10. Departures from multica

| multica | agentwork | rationale |
|---|---|---|
| server/daemon split + PostgreSQL | single process + SQLite | single machine, single user; no need for an external authority |
| workspace / auth / multi-tenancy | none | single user |
| server arbitrates run-terminal state | **goal state machine internalizes authority** | without an external server, authority must be internalized to be self-consistent |
| reassignment doesn't abort the old run, both parallel (MUL-4113) | also doesn't abort, but discards the old result via goal arbitration — correctness doesn't depend on the old run | digests the historical baggage: self-consistent without an external authority |
| one backend per provider (17 native adapters) | backend abstraction + three protocol families (acp/jsonl/jsonrpc), ACP first | no need for 17 backends on day one; the interface is in place |
| global daemon-level concurrency pool | per-agent semaphore | agent = persona + resource quota; per-agent fits better |
| run writes issue status directly | **StartRun/CompleteRun never touch goal status** | enforced at the design level rather than by convention |
| skill system | deferred (agent system_prompt carries SOPs) | single-user MVP can add later |
| inbox / notifications | deferred (`@all`'s "notify humans" degenerates for single user) | add later |
| 28 tables | ~11 tables | drop workspace/auth/team/chat/inbox/notification/project/label/dependency |