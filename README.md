# agentwork

> [中文](README.zh.md) | [Design](DESIGN.md) | [协作模型](Collaboration.v2.md)

An **AI task pipeline OS** — a single-user control plane that runs CLI agents
unattended through the full loop: goal → execution → machine verification →
human gate → delivery. You define acceptance policies in natural language,
agents do the work, the platform judges, and you only appear at checkpoints.

**Single process, single machine, single user, no auth.** Local SQLite for
state, in-process event bus, Go daemon + Next.js web UI.

## Highlights

- **Full unattended loop** — goals, runs, machine verification, structural
  guards, human gates, automatic delivery (merge + re-verify + push)
- **Acceptance policy per domain** — you state "what done means" in natural
  language; a processor agent compiles it into executable checks (verify
  commands, guards, gates) which you confirm once
- **Sub-goals with changes** — the owner splits work into sub-goals (own
  assignee, own worktree, machine or agent verification); verified sub-goals
  produce **Changes** with revisions the owner integrates — conflicts wake
  the assignee to rework automatically
- **Four collaboration behaviors** — Comment / Consult / Handoff / Sub-goal,
  exposed as MCP tools with owner-only permissions
- **Multi-protocol & transport** — ACP / JSONL / JSON-RPC over stdio / ws / tcp;
  agents can carry their own extra MCP servers
- **Triggers** — Web, cron schedules, GitHub/GitCode issues (webhook + poll),
  Feishu IM inbound (@-bot task creation)
- **Feishu notifications** — approval cards (approve/reject right in IM),
  completion/failure pushes, a daily digest
- **Real-time web UI** — live event stream, execution timeline, sub-goal /
  change / verification panels

> **One-line mental model:** a **goal** is a work item and the sole holder of
> state authority; a **run** is one agent's turn on it — runs report up, never
> decide. The platform judges completion; you hold the gates.

## Quick start

### Prerequisites

- Go 1.26+
- Node.js 20+
- npm 10+

### Backend

```bash
go build -o agentwork-daemon ./cmd/agentwork-daemon
go build -o agentwork-cli ./cmd/agentwork-cli

# Start the daemon (default :7373)
./agentwork-daemon
```

### Frontend

```bash
cd web
npm install
npm run build && npm start
```

### First goal

1. Create a **domain** (项目): repo URL + acceptance policy in natural language
   (or leave it unfrozen to force a human checkpoint)
2. Create a **runtime** (e.g. `opencode acp --pure`, stdio + acp) and an
   **agent**
3. Create a **goal**, assign the agent, and watch the loop run: execution →
   verification → review → your approval → automatic delivery

## Core concepts

| Concept | What it is |
|---|---|
| **Domain** (Project) | An asset/evolution domain: shared repo + acceptance policy (NL intent compiled into checks) + default gates. |
| **Goal** | A work item (product plane). Status: backlog → active → review → done / failed / cancelled. The sole holder of state authority. |
| **Run** | One agent's turn on a goal. Status: queued → running → completed / failed / cancelled, plus a **role** (owner / subgoal / consult / review / verify). No authority — it reports to the goal layer. |
| **Runtime** | A launch spec — transport (stdio/ws/tcp) + provider (acp/jsonl/jsonrpc) + executable/args or endpoint + env. |
| **Agent** | A runtime + persona (system prompt / model / env / extra MCP servers / max concurrency). |
| **Squad** | A routing group that does no work itself: goals route to its leader, who delegates via sub-goals; members with role=reviewer are auto-pulled into review checkpoints. |
| **Sub-goal** | A work item split off a goal (not a child goal — no recursion). Own assignee + optional agent verifier; machine retries (≤3) and verifier rejections are counted separately. |
| **Change** | A sub-goal's logical deliverable: ready → integrating → integrated, or conflict → assignee rework → revision N+1. The owner integrates it via the `integrate_change` tool. |
| **Verifier** | Machine (domain verify commands) by default, or a named agent issuing structured verdicts (`verify_sub_goal`). |
| **Gates** | Human checkpoint rules from the acceptance policy (merge, diff_contains, ...); weak verification forces a checkpoint. |
| **Mention** | The Consult primitive: a structured URI in a comment pulls an agent into a read-only guest run; the requester resumes automatically after the answer. |
| **Schedule** | A cron template that clones a goal per firing (idempotent, timezone-aware). |
| **Trigger** | Web, cron, GitHub/GitCode issues (webhook + poll, deduped by source ref), Feishu IM inbound. |

## Collaboration (four behaviors)

| Behavior | Tool | Who may |
|---|---|---|
| **Comment** (say) | `comment_goal` | anyone |
| **Consult** (ask) | `consult_agent` | the goal's owner |
| **Handoff** (transfer) | `handoff_goal` | the goal's owner |
| **Sub-goal** (split) | `create_sub_goal` / `cancel_sub_goal` | the goal's owner |

Agents coordinate exclusively through the `agentwork` MCP tools advertised at
every session (plus `verify_sub_goal`, `integrate_change`, `get_change`,
`get_sub_goal`, `get_verification`, and the `*_list` tools). Full model in
[Collaboration.v2.md](Collaboration.v2.md).

## Architecture

```
                    ┌────────────── HTTP API + WS hub ──────────┐
                    │   goals/runs/sub-goals/changes/…           │
                    ▼                                             ▼
              service layer (state authority)             web UI (Next.js)
                    │  bus.Publish (after commit)                │
                    ▼                                            │
               SQLite (truth) ── daemon scheduler ───────────────┤
                                    │ claim run → runTask        │
                                    ▼                            │
                             runtime.Open(spec)                  │
                                    │  acp | jsonl | jsonrpc     │
                                    ▼                            │
                             agent CLI subprocess                │
                              (stdio/ws/tcp)                     │
                              └─ fs/terminal RPC + agentwork MCP  │
                                 (worktree, tools)               │
                    ┌───────────────┬────────────────────────────┘
                    ▼               ▼
             machine verify /   review gate → deliver
             guards (domain)    (human approve → merge+re-verify+push)
```

- **Event ≠ truth** — the bus is a wakeup hint; every transition is a
  conditional DB transaction, idempotent under replay
- **Per-run worktrees** — `~/.agentwork/runs/<runID>` ephemeral git worktrees
  against per-domain bare repos; crash recovery replays terminal runs and
  re-derives attention at startup
- **Coordinator** — derived OwnerAttention (integration / recovery /
  user_action) wakes the owner exactly when there is work for it

## CLI tool

`agentwork-cli` is a human debugging tool (agents use the MCP tools). It needs
`AGENTWORK_SERVER_URL` (or runs against `http://localhost:7373` by default):

```bash
agentwork-cli goal list --limit 5
agentwork-cli stats
```

## Tech stack

| Layer | Stack |
|---|---|
| **Backend** | Go 1.26, SQLite (modernc), gorilla/websocket, robfig/cron, MCP go-sdk |
| **Frontend** | Next.js 16, React 19, Tailwind CSS 4, TanStack React Query |
| **Protocols** | ACP, JSONL, JSON-RPC |
| **Transports** | stdio, WebSocket, TCP |
| **Notifications** | Feishu (approval cards, digest, IM inbound) |
| **Issue triggers** | GitHub, GitCode |
