-- agentwork schema. Run once at startup (CREATE TABLE IF NOT EXISTS).
-- Single-user, single-file SQLite (WAL mode).
-- foreign_keys + journal_mode are set per-connection via the DSN in store.go
-- (they are per-connection pragmas, so setting them here only affects the one
-- connection that runs this file). Kept here for visibility/documentation.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- A runtime is a launch spec: how to connect to an ACP-speaking agent.
-- Pure configuration; no capabilities of its own.
-- transport=stdio → daemon spawns executable+args (subprocess).
-- transport=ws|tcp → daemon dials endpoint (remote service; no spawn).
CREATE TABLE IF NOT EXISTS runtime (
    id          TEXT PRIMARY KEY,           -- uuid8
    name        TEXT NOT NULL UNIQUE,
    transport   TEXT NOT NULL DEFAULT 'stdio', -- stdio|ws|tcp
    executable  TEXT NOT NULL DEFAULT '',   -- stdio: "/path/to/openagent-cli"; ws/tcp: ''
    args        TEXT NOT NULL DEFAULT '[]', -- stdio: JSON array e.g. ["serve","--acp"]; ws/tcp: '[]'
    endpoint    TEXT NOT NULL DEFAULT '',   -- ws/tcp: "ws://host:port" or "host:port"; stdio: ''
    env         TEXT NOT NULL DEFAULT '{}', -- JSON object of env vars (stdio only)
    protocol    TEXT NOT NULL DEFAULT 'acp',
    created_at  TEXT NOT NULL               -- RFC3339
);

-- An agent is a runtime + a persona. Creating an agent launches a
-- long-lived ACP server subprocess; status reflects that subprocess.
CREATE TABLE IF NOT EXISTS agent (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL DEFAULT '',
    runtime_id      TEXT NOT NULL REFERENCES runtime(id),
    system_prompt   TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',  -- optional override
    workdir_base    TEXT NOT NULL DEFAULT '',  -- per-agent base; sessions use workdir_base/<task_id>
    env             TEXT NOT NULL DEFAULT '{}', -- agent-level env, layered over runtime env
    max_concurrent  INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'offline', -- online|offline|crashed
    pid             INTEGER NOT NULL DEFAULT 0,      -- current ACP server pid
    created_at      TEXT NOT NULL
);

-- A task is the unit of work. assignee is polymorphic (agent | human).
-- The ACP session id is server-generated and stored in the session table
-- (composite key with agent_id); it is not derived from the task id.
CREATE TABLE IF NOT EXISTS task (
    id              TEXT PRIMARY KEY,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    parent_id       TEXT REFERENCES task(id),       -- sub-task
    assignee_type   TEXT NOT NULL DEFAULT 'agent',   -- agent | human
    assignee_id     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'backlog', -- backlog|queued|running|waiting_children|completed|failed|cancelled
    handoff_note    TEXT NOT NULL DEFAULT '',
    created_by_type TEXT NOT NULL DEFAULT 'human',
    created_by_id   TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);

-- task_queue is the dispatch buffer. A row here means "this task should
-- run on this agent". daemon claims rows, runs them, updates status.
-- Only state machine + associations + result summary live here; full
-- conversation lives in chat_message.
CREATE TABLE IF NOT EXISTS task_queue (
    id             TEXT PRIMARY KEY,
    task_id        TEXT NOT NULL REFERENCES task(id),
    agent_id       TEXT NOT NULL REFERENCES agent(id),
    status         TEXT NOT NULL DEFAULT 'queued', -- queued|running|completed|failed
    attempt        INTEGER NOT NULL DEFAULT 1,      -- increments on crash-retry
    result_summary TEXT NOT NULL DEFAULT '',
    queued_at      TEXT NOT NULL,
    started_at     TEXT NOT NULL DEFAULT '',
    finished_at    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_task_queue_status ON task_queue(status);
CREATE INDEX IF NOT EXISTS idx_task_parent ON task(parent_id);
CREATE INDEX IF NOT EXISTS idx_task_assignee ON task(assignee_type, assignee_id);

-- A session is one agent's ACP session for one task. session_id is the
-- ACP server-returned id; it is unique only within one agent, so the
-- primary key is (session_id, agent_id). A task handed off across agents
-- has one row per agent; only the current assignee's row is active.
CREATE TABLE IF NOT EXISTS session (
    session_id     TEXT NOT NULL,
    agent_id       TEXT NOT NULL REFERENCES agent(id),
    task_id        TEXT NOT NULL REFERENCES task(id),
    workdir        TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'active', -- active|frozen|closed
    created_at     TEXT NOT NULL,
    PRIMARY KEY (session_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_session_task ON session(task_id, status);

-- Redundant copy of the ACP conversation so the frontend can query
-- directly and a handoff target can read history even if the source
-- agent is offline. The agent runtime remains the source of truth
-- (agentwork can replay via session/load); this is a convenience cache.
CREATE TABLE IF NOT EXISTS chat_message (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES task(id),
    role       TEXT NOT NULL,              -- user|assistant|tool|system
    content    TEXT NOT NULL DEFAULT '',
    tool_calls TEXT NOT NULL DEFAULT '[]', -- JSON
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_message_task ON chat_message(task_id, created_at);

-- Append-only audit trail. Handoffs, child creations, crashes, etc.
CREATE TABLE IF NOT EXISTS activity_log (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES task(id),
    actor_type TEXT NOT NULL,              -- agent|human|system
    actor_id   TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,              -- created|assigned|handoff|child_created|child_done|crashed|...
    detail     TEXT NOT NULL DEFAULT '{}', -- JSON
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_activity_task ON activity_log(task_id, created_at);

-- A schedule is a cron-triggered task template. At each cron occurrence the
-- daemon clones a fresh task row from the template fields, assigns it to the
-- agent, and enqueues it — the normal dispatch chain then runs it. This is the
-- "template + instance" model (like multica autopilot create_issue): every
-- firing produces an independent task with its own history/retry, so the task
-- state machine is untouched.
CREATE TABLE IF NOT EXISTS schedule (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    title_template  TEXT NOT NULL,             -- cloned task title
    description     TEXT NOT NULL DEFAULT '',  -- cloned task description
    assignee_id     TEXT NOT NULL REFERENCES agent(id),
    cron_expression TEXT NOT NULL,             -- 5-field standard cron
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    enabled         INTEGER NOT NULL DEFAULT 1,
    next_run_at     TEXT NOT NULL DEFAULT '',  -- next fire time RFC3339; '' = unset
    last_run_at     TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_schedule_enabled ON schedule(enabled, next_run_at);

-- schedule_run records each firing and the task it produced (history +
-- idempotency: one firing per (schedule_id, planned_at)).
CREATE TABLE IF NOT EXISTS schedule_run (
    id          TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL REFERENCES schedule(id),
    task_id     TEXT NOT NULL REFERENCES task(id),
    planned_at  TEXT NOT NULL,                 -- cron occurrence that fired
    status      TEXT NOT NULL DEFAULT 'dispatched', -- dispatched|failed
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_schedule_run_schedule ON schedule_run(schedule_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_schedule_run_planned ON schedule_run(schedule_id, planned_at);
