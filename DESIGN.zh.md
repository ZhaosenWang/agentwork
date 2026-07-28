# agentwork — 设计文档

> Task 驱动的多 runtime agent 管理平台。把 CLI agent（openagent-cli / opencode / codex …）当作可调度的 runtime，统一管理 task 的创建、指派、执行、协调（转交 / sub-task）。

---

## 1. 核心概念

| 概念 | 是什么 | 存在哪 |
|---|---|---|
| **runtime** | 启动定义——怎么连一个说 ACP 的程序。`transport`（stdio/ws/tcp）+ `executable`+`args`（stdio）或 `endpoint`（ws/tcp）+ `env` + `protocol`。纯配置，无能力。 | `runtime` 表 |
| **agent** | runtime + 人设（system_prompt / model / workdir / env / max_concurrent）。并发单元：每个 agent 有一个带信号量的 worker。 | `agent` 表 |
| **task** | 工作单元，指派给 agent 或 human。有状态机，可选 `parent_id` 做 sub-task 协调。 | `task` 表 |
| **task_queue** | 调度缓冲。一行 = "这个 task 该在这个 agent 上跑"。daemon 认领并执行。 | `task_queue` 表 |

### runtime = 启动定义，agent = runtime + 人设

**runtime** 行是完整的启动定义（"定义即运行"）：daemon 读它、连它，不需要额外配置。三种 transport：

- `stdio` — spawn `executable` + `args` 子进程，stdin/stdout 跑 ACP。
- `ws` — dial WebSocket `endpoint`，text frame 跑 ACP。
- `tcp` — dial TCP `endpoint`，换行分隔 JSON 跑 ACP。

**agent** 选一个 runtime（`runtime_id` FK），加人设：system prompt、model 覆盖、workdir 基目录、agent 级 env、并发上限。

### per-task 连接模型

每次 task 运行开一个**全新**连接，跑一个 ACP `Prompt`，drain 完事件，拆连接。"唤醒父 task" = 重新入队父 task 并带唤醒 prompt，不是往活着的 session 追加消息。长驻 session 模型（一个 agent 上跨多次 Prompt 复用 `session/load` / `session/resume`）是未来方向；`session` 表和 `agent.status`/`agent.pid` 列为它保留。

---

## 2. 架构

```
               HTTP API (internal/server)
               ┌──────────────────────────────────────┐
  前端 ──────► │ /runtimes /agents /tasks /schedules   │
  CLI ──────► │ /tasks/{id}/assign|cancel|wait|messages│
               │ /ws (WebSocket fan-out) /healthz       │
               └──────────────────┬───────────────────┘
                                  │ handler.Handlers
               ┌──────────────────▼───────────────────┐
               │          service 层                   │
               │  Task   Agent   Runtime   Schedule    │  ← CRUD + 业务规则
               └──────────────────┬───────────────────┘
                                  │ 写 DB + 发事件
               ┌──────────────────▼───────────────────┐
               │          store (SQLite WAL)            │
               │  task task_queue agent runtime         │
               │  session chat_message activity_log     │
               │  schedule schedule_run                 │
               └──────────────────────────────────────┘
                                  │
               ┌──────────────────▼───────────────────┐
               │          daemon (调度器)               │
               │  • 轮询 task_queue → 认领 → 执行        │
               │  • 轮询 schedule → 触发 → 建 task       │
               │    + 入队                              │
               │  • runtime.Launch (stdio/ws/tcp) →     │
               │    acp.Session → Initialize → Prompt   │
               │  • 重启时回收卡住的 'running'           │
               └──────────────────────────────────────┘
                                  │
               ┌──────────────────▼───────────────────┐
               │     events.Bus (进程内 pub/sub)        │
               │  task:* agent:* schedule:*             │ → WS hub → 前端
               └──────────────────────────────────────┘
```

### 分层

- **store** — SQLite（modernc.org/sqlite，纯 Go，无 cgo）。单文件，WAL 模式。DSN pragma 把 `foreign_keys=ON`、`journal_mode=WAL`、`busy_timeout=5000` 传播到连接池每个连接。启动时 `CREATE TABLE IF NOT EXISTS` 建 schema，无迁移工具。
- **service** — 业务逻辑。每个 service（Task/Agent/Runtime/Schedule）管自己的 CRUD + 校验 + 事件发布。`TaskService` 还管 sub-task 协调（`WaitChildren`、`WakeupParentIfReady`）。
- **daemon** — 调度器。管 per-agent worker、task_queue dispatch 循环、schedule 触发、`runTask`（launch runtime → ACP 握手 → Prompt → drain 事件 → 记录结果）。订阅 `agent:created`/`agent:deleted` 管 worker 生命周期。
- **server** — HTTP + WebSocket 边界。挂 CRUD 路由 + WS hub。service 在这里装配；daemon 只通过共享的 `events.Bus` 耦合。
- **events.Bus** — 进程内 pub/sub。handler 在各自 goroutine 跑，有 panic recovery。无持久化、无跨节点。通知 WS hub（前端）和 daemon（worker 生命周期）。
- **runtime** — transport 层。`runtime.Launch(ctx, Spec, taskEnv)` 读 runtime 定义，开连接（stdio spawn / ws dial / tcp dial），返回 `*acp.Session`。"定义即运行"。
- **acp** — ACP v1 协议层（JSON-RPC 2.0）。`acp.Session` transport 无关：在任意 `io.Reader` + `io.Writer` 上跑协议。`ConnectStdio` 是 stdio transport 的便捷工厂。

### 二进制

- `agentwork-daemon` — HTTP server + 内嵌 daemon。单进程。
- `agentwork-cli` — agent 侧工具。daemon 把它注入 agent 子进程的 `PATH`，加 `AGENTWORK_SERVER_URL` / `AGENTWORK_TASK_ID` / `AGENTWORK_AGENT_ID` 环境变量，让 agent 回调产生结构化副作用（转交、建子 task、列 task、追加消息、wait-children）。CLI-as-tool。

---

## 3. Task 生命周期

```
backlog ──assign──► queued ──claim──► running ──┬──► completed
                   │                  │          │
                   │                  ├──► failed (重试次数用完)
                   │                  │
                   │                  ├──► retrying ──► queued (attempt+1)
                   │                  │
                   │                  └──► waiting_children ──子任务全完成──► queued
                   │
                   └──cancel──► cancelled
```

- **backlog** — 停车坪。指派了但没入队，不跑。
- **queued** — 在 `task_queue` 里，等 daemon 认领。
- **running** — daemon 认领了，ACP session 活着。
- **waiting_children** — agent 调了 wait-children；父 task 挂起，等所有非终态子任务完成。
- **completed / failed / cancelled** — 终态。

### 调度链

```
task 指派给 agent + status≠backlog
  → enqueueInTx: INSERT task_queue (status=queued) + UPDATE task status=queued
  → daemon 轮询 task_queue (claimQueued: 原子 UPDATE…RETURNING)
  → 路由到 per-agent worker (信号量限并发)
  → runTask: runtime.Launch → acp Initialize → NewSession → Prompt
  → drain EventHandler (OnAgentMessage/OnToolCall/…) → 落 chat_message + 发事件
  → finishTask: UPDATE task_queue + task status, 清 handoff_note, 触发 WakeupParentIfReady
```

### Sub-task 协调

1. 父 agent 调 `agentwork-cli task create --parent <parent_id>` 建子任务。
2. 父 agent 调 `agentwork-cli task wait-children` → 父 status = `waiting_children`。
3. 每个子任务独立走正常调度链。
4. 子任务到终态时，`WakeupParentIfReady` 检查：父是 `waiting_children`？所有子任务都终态了？（在一个事务里查，防竞态）。是 → 重新入队父 task，带子任务结果摘要的唤醒 prompt。
5. 父 task 第二次跑，看到唤醒 prompt，继续。

等待集是**动态**的：调 wait-children 之后建的子任务也算。唤醒时检查"当前所有非终态子任务"，不是调 wait 时的快照。

### 转交（handoff）

同一个 task 从 agent A 转给 agent B：
- A 的 session 冻结（`status='frozen'`），历史保留。
- B 开新 session，开场 prompt = `handoff_note` + A 的 `chat_message` 历史。
- 在 `TaskService.Assign` 实现。

---

## 4. Schedule（cron）

Schedule 是 cron 触发的 task 模板。daemon 扫到期的 schedule，在一个事务里触发：

```
INSERT task (status=queued, created_by=system)
INSERT task_queue (status=queued)
INSERT schedule_run (planned_at)  ← 唯一索引 uq_schedule_run_planned 保证幂等
```

如果并发 tick 已经触发了同一个 `(schedule_id, planned_at)`，唯一索引拒绝插入，事务回滚（不留孤儿 task），`next_run_at` 前进。这是"外部事件 → 自动建 task → 入队"的模式——webhook / issue 接入同构。

---

## 5. 数据模型

```sql
runtime(id, name, transport, executable, args, endpoint, env, protocol, created_at)
  -- transport: stdio|ws|tcp
  -- stdio 用 executable+args；ws/tcp 用 endpoint

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
  -- session_id 是 ACP server 生成的，只在单个 agent 内唯一

chat_message(id, task_id, role, content, tool_calls, created_at)
activity_log(id, task_id, actor_type, actor_id, action, detail, created_at)
schedule(id, name, title_template, description, assignee_id, cron_expression,
         timezone, enabled, next_run_at, last_run_at, created_at)
schedule_run(id, schedule_id, task_id, planned_at, status, created_at)
  -- UNIQUE(schedule_id, planned_at) 保证幂等触发
```

---

## 6. Transport / 协议分离

```
runtime.Launch(spec)           ← transport 层: stdio spawn / ws dial / tcp dial
  │
  ▼
acp.NewSession(r, w, closeFn)  ← 协议层: JSON-RPC 2.0 over io.Reader/Writer
  │
  ▼
sess.Initialize / NewSession / Prompt / …  ← ACP v1 方法，transport 无关
```

`acp.Session` 只持 `io.Reader` + `io.Writer` + 一个 close 函数——没有进程句柄、没有 socket。transport 层（`internal/runtime`）负责建读写对和清理闭包。加新 transport 就在 `runtime.Launch` 加一个 `case`，协议层不动。

---

## 7. 扩展点

- **新 HTTP 路由** — `handler.Mount` 加一行 + 一个方法。service 在 `server.ListenAndServe` 装配。
- **新 transport** — `runtime.Launch` 加一个 `case`。
- **新事件类型** — `bus.Publish` + 把 topic 加进 `ws/hub.go` 的 topics 列表。
- **外部触发 → task**（webhook、issue 轮询 …）— 调 `TaskService.Create`，传 `Status=queued` + agent 指派。复用 `enqueueInTx` 和整条调度链。schedule 是现成先例。幂等要靠外部事件 id 的唯一约束（schedule 用 `uq_schedule_run_planned`，webhook 要自己的）。
- **非 ACP 协议** — `runtime.protocol` 列保留；目前所有 transport 都说 ACP。分支在 `runtime.Launch` 里加。

### 不是扩展点（要改结构）

- **多节点** — `events.Bus` 是进程内，无跨节点。单 daemon + 单 DB。多节点要外部消息总线。
- **推送式调度** — daemon 轮询 `task_queue`。要更低延迟可以换进程内 channel 或 SQLite notify，调度逻辑（`claimQueued`）不变。

---

## 8. 技术栈

- **Go** + `net/http` ServeMux（Go 1.22+ method+path 模式）+ 手写 SQL
- **SQLite** via `modernc.org/sqlite`（纯 Go，无 cgo），WAL 模式
- **ACP v1** client SDK 在 `internal/acp/`（从 openagent-go vendor 的拷贝；上游改了要手动同步）
- **gorilla/websocket** 用于 WS hub 和 ws transport
- **robfig/cron/v3** 用于 schedule cron 表达式
- **Vue 3 + Element Plus** 前端
- 单用户、无鉴权——开源给自己管自己的 agent
