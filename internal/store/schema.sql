-- agentwork schema. Run once at startup (CREATE TABLE IF NOT EXISTS).
-- Single-user, single-file SQLite (WAL mode).
-- foreign_keys + journal_mode are set per-connection via the DSN in store.go
-- (they are per-connection pragmas, so setting them here only affects the one
-- connection that runs this file). Kept here for visibility/documentation.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- Two-layer coordination model (see DESIGN.zh.md §4):
--   goal  = a work item (product plane). The SOLE holder of state authority.
--   run   = one execution of a goal by one agent (execution plane). No
--           authority — on terminal status it reports up to the goal layer
--           via GoalService.reconcileOnRunEnd, which is the only place that
--           changes goal.status.
-- A goal can be executed many times / by many agents in succession; full
-- history is retained across runs.

-- A runtime is a launch spec: how to connect to a protocol-speaking agent.
-- Pure configuration; no capabilities of its own.
-- transport=stdio → daemon spawns executable+args (subprocess).
-- transport=ws|tcp → daemon dials endpoint (remote service; no spawn).
-- provider selects which backend speaks the agent's wire protocol:
--   acp → JSON-RPC 2.0 (internal/proto/acp)
--   jsonl → single-direction JSONL stream (claude/opencode style)
--   jsonrpc → bidirectional JSON-RPC 2.0 (codex app-server style)
CREATE TABLE IF NOT EXISTS runtime (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    transport   TEXT NOT NULL DEFAULT 'stdio', -- stdio|ws|tcp
    provider    TEXT NOT NULL DEFAULT 'acp',   -- acp|jsonl|jsonrpc → which backend
    executable  TEXT NOT NULL DEFAULT '',      -- stdio: "/path/to/cli"; ws/tcp: ''
    args        TEXT NOT NULL DEFAULT '[]',    -- stdio: JSON array; ws/tcp: '[]'
    endpoint    TEXT NOT NULL DEFAULT '',      -- ws/tcp: "ws://host:port" or "host:port"; stdio: ''
    env         TEXT NOT NULL DEFAULT '{}',    -- JSON object of runtime env (stdio only)
    created_at  TEXT NOT NULL                  -- RFC3339
);

-- An agent is a runtime + a persona. Creating an agent does NOT launch a
-- process under the per-task model: a fresh connection is opened per run.
-- agent.status/pid columns are deliberately absent here — they belong to the
-- future long-lived-session model and would be dead columns today.
CREATE TABLE IF NOT EXISTS agent (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL DEFAULT '',
    runtime_id      TEXT NOT NULL REFERENCES runtime(id),
    system_prompt   TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',  -- optional override
    workdir_base    TEXT NOT NULL DEFAULT '',  -- per-agent base; runs use workdir_base/<run_id>
    env             TEXT NOT NULL DEFAULT '{}', -- agent-level env, layered over runtime env
    max_concurrent  INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL
);

-- A squad is a routing group. It does no work itself: assigning a goal to a
-- squad or @mentioning a squad routes only to its leader agent, who then
-- delegates by handing off work to members. Designed per multica §7 (no member
-- fan-out). The leader must be an agent; squads cannot nest.
CREATE TABLE IF NOT EXISTS squad (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    leader_id    TEXT NOT NULL REFERENCES agent(id),  -- must be an agent
    instructions TEXT NOT NULL DEFAULT '',            -- extra briefing for leader runs
    created_at   TEXT NOT NULL
);

-- squad_member is polymorphic: member_type discriminates agent vs human.
-- member_id has no single FK constraint (the discriminator is the CHECK);
-- the application resolves the relationship. No nesting ('squad' not allowed).
CREATE TABLE IF NOT EXISTS squad_member (
    id          TEXT PRIMARY KEY,
    squad_id    TEXT NOT NULL REFERENCES squad(id) ON DELETE CASCADE,
    member_type TEXT NOT NULL CHECK (member_type IN ('agent', 'human')),
    member_id  TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    UNIQUE(squad_id, member_type, member_id)
);

-- goal: a work item (product plane). The sole holder of state authority.
-- assignee is polymorphic (agent | squad | human).
-- Coordination is via @mention in comments (the comment thread is the workflow
-- record); there is no sub-goal hierarchy. See DESIGN.zh.md §5.
CREATE TABLE IF NOT EXISTS goal (
    id              TEXT PRIMARY KEY,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    assignee_type   TEXT NOT NULL DEFAULT 'agent',   -- agent | squad | human
    assignee_id     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'backlog', -- backlog|active|done|failed|cancelled
    handoff_note    TEXT NOT NULL DEFAULT '',
    created_by_type TEXT NOT NULL DEFAULT 'human',   -- human | agent
    created_by_id   TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_goal_assignee ON goal(assignee_type, assignee_id);
CREATE INDEX IF NOT EXISTS idx_goal_status ON goal(status);

-- run: one execution of a goal by one agent (execution plane). No authority:
-- on terminal status the daemon calls GoalService.reconcileOnRunEnd(run),
-- which checks whether this run still belongs to the current assignee before
-- touching goal.status. status here is the execution-plane state machine.
CREATE TABLE IF NOT EXISTS run (
    id                 TEXT PRIMARY KEY,
    goal_id            TEXT NOT NULL REFERENCES goal(id),
    agent_id           TEXT NOT NULL REFERENCES agent(id),
    session_id         TEXT NOT NULL DEFAULT '',  -- protocol-returned; for history/future resume
    workdir            TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'queued', -- queued|running|completed|failed|cancelled
    attempt            INTEGER NOT NULL DEFAULT 1,
    result_summary     TEXT NOT NULL DEFAULT '',
    trigger_comment_id TEXT NOT NULL DEFAULT '',  -- which comment caused this run (mention/child-done)
    is_leader_run      INTEGER NOT NULL DEFAULT 0,-- 1 if a squad leader run
    squad_id           TEXT NOT NULL DEFAULT '',  -- the squad a leader run belongs to
    queued_at          TEXT NOT NULL,
    started_at         TEXT NOT NULL DEFAULT '',
    finished_at        TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_goal ON run(goal_id);
CREATE INDEX IF NOT EXISTS idx_run_agent ON run(agent_id);
CREATE INDEX IF NOT EXISTS idx_run_status ON run(status);

-- comment: a message under a goal. Authors are polymorphic (human | agent |
-- system). content is Markdown and may carry structured mention URIs:
--   [@Name](mention://agent/<uuid>)   → enqueue a new run on that agent
--   [@Name](mention://squad/<uuid>)   → route to the squad's leader
--   [@Name](mention://human/<uuid>)   → renders a link, no run
--   [@all](mention://all/all)         → suppress auto-trigger, notify humans
-- Mentions are parsed only from persisted comment bodies, never from agent
-- stdout (the agent drives triggers by calling agentwork-cli).
CREATE TABLE IF NOT EXISTS comment (
    id          TEXT PRIMARY KEY,
    goal_id     TEXT NOT NULL REFERENCES goal(id),
    author_type TEXT NOT NULL CHECK (author_type IN ('human', 'agent', 'system')),
    author_id   TEXT NOT NULL DEFAULT '',   -- zero id for system rows
    parent_id   TEXT REFERENCES comment(id), -- one level of threading
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_comment_goal ON comment(goal_id, created_at);

-- chat_message: the run's output stream cache (tool calls, thoughts, text),
-- for the run detail view. Belongs to a run, not a goal.
CREATE TABLE IF NOT EXISTS chat_message (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES run(id),
    role       TEXT NOT NULL,              -- user|assistant|tool|system
    content    TEXT NOT NULL DEFAULT '',
    tool_calls TEXT NOT NULL DEFAULT '[]', -- JSON
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_message_run ON chat_message(run_id, created_at);

-- Append-only audit trail. Handoffs, child creations, squad evaluations, ...
CREATE TABLE IF NOT EXISTS activity_log (
    id         TEXT PRIMARY KEY,
    goal_id    TEXT NOT NULL REFERENCES goal(id),
    actor_type TEXT NOT NULL,              -- agent|human|system
    actor_id   TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,             -- created|assigned|handoff|child_created|children_done|squad_leader_evaluated|cancelled|...
    detail     TEXT NOT NULL DEFAULT '{}', -- JSON
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_activity_goal ON activity_log(goal_id, created_at);

-- A schedule is a cron-triggered goal template. At each cron occurrence the
-- daemon clones a fresh goal row from the template fields, assigns it, and
-- enqueues a run — the normal dispatch chain then runs it. (Template + instance
-- model, like multica autopilot.) Idempotency via uq_schedule_run_planned.
CREATE TABLE IF NOT EXISTS schedule (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    title_template  TEXT NOT NULL,             -- cloned goal title
    description     TEXT NOT NULL DEFAULT '',  -- cloned goal description
    assignee_type   TEXT NOT NULL DEFAULT 'agent', -- agent|squad
    assignee_id     TEXT NOT NULL,
    cron_expression TEXT NOT NULL,             -- 5-field standard cron
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    enabled         INTEGER NOT NULL DEFAULT 1,
    next_run_at     TEXT NOT NULL DEFAULT '',  -- next fire time RFC3339; '' = unset
    last_run_at     TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_schedule_enabled ON schedule(enabled, next_run_at);

-- schedule_run records each firing and the goal it produced (history +
-- idempotency: one firing per (schedule_id, planned_at)).
CREATE TABLE IF NOT EXISTS schedule_run (
    id          TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL REFERENCES schedule(id),
    goal_id     TEXT NOT NULL REFERENCES goal(id),
    planned_at  TEXT NOT NULL,                 -- cron occurrence that fired
    status      TEXT NOT NULL DEFAULT 'dispatched', -- dispatched|failed
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_schedule_run_schedule ON schedule_run(schedule_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_schedule_run_planned ON schedule_run(schedule_id, planned_at);