# agentwork — 设计文档

> Task 驱动的多协议 agent 管理平台。把 CLI agent（claude / codex / opencode / openagent-cli …）当作可调度的 runtime，统一管理 goal 的指派、执行、协调（转交 / sub-goal / @mention / squad）。
>
> 单进程、单机、单用户，无鉴权。本地 SQLite 持久化，进程内事件总线。
>
> 本设计为 agentwork 从 0 立定，不照搬参照实现的历史包袱。

---

## 1. 一句话心智模型

> **goal 是工作条目（谁负责、推进到哪），run 是一次执行（某个 agent 跑了一轮）。同一个 goal 可被多次执行、多 agent 接力，完整历史保留。run 无权威，状态由 goal 层裁决。**

agent 之间不直接对话。所有协调都通过读写同一个 goal（comment / handoff note / 状态 / 共享 workdir）异步、队列驱动。agent 主动调 `agentwork-cli` 产生**结构化副作用**——从来不让系统去解析 agent 输出提取意图。

---

## 2. 核心概念

| 概念 | 是什么 | 存在哪 |
|---|---|---|
| **runtime** | 进程形态——怎么连一个说某种协议的程序。`transport`（stdio/ws/tcp）+ `executable`+`args`（stdio）或 `endpoint`（ws/tcp）+ `env`。纯配置，无能力。 | `runtime` 表 |
| **provider** | 协议种类——说哪种协议，决定挂哪个 backend。`acp` / `jsonl` / `jsonrpc`。runtime 带 provider 字段。 | `runtime.provider` |
| **agent** | runtime + 人设（system_prompt / model / workdir / env / max_concurrent）。并发单元：每 agent 一个 worker + 信号量。 | `agent` 表 |
| **squad** | 路由组，不跑活。有自己的 leader（一个 agent）。指派给 squad / @mention squad 都只路由到 leader，由 leader 委派子 goal。 | `squad` 表 |
| **goal** | 工作条目，产品面。可指派给 agent / squad / human。有状态机，可选 `parent_id` 做 sub-goal 协调。**状态权威的唯一持有者。** | `goal` 表 |
| **run** | 一次执行，执行面。一个 agent 对一个 goal 跑一轮。`run` 表是 goal 的执行投影。**无权威，终态只向 goal 层汇报。** | `run` 表 |

### 双层模型的设计本质

双层不是"把 task 拆开"这个动作本身，而是**状态权威归谁**：

- **run 层无权威**：run 只是"某 agent 对某 goal 跑了一轮"的执行记录。run 到达终态（completed/failed/cancelled）时**不直接写 goal 状态**，只向 goal 层汇报"我跑完了，附结果"。
- **goal 层是唯一权威**：goal 状态机自己决定"这活算不算完 / 还要不要再开一轮 / 转给谁"。任何影响 goal 走向的写入，都走 goal 层仲裁方法 `reconcileOnRunEnd(run)`，内部自查当前状态再裁决。

### 为什么这样分层

agentwork 从 0 设计，把权威**内化到 goal 状态机**：run 完成需经 goal 层仲裁，而 goal 层会自查"提交结果的 run 是不是当前 owner 持有的"，不是就丢弃。没有外部权威也能自洽，无需纠结"旧 run 中止还是并行"——**旧 run 中止不中止都不影响正确性**，它的结果没有改 goal 状态的资格。

---

## 3. 架构

```
┌──────────────────────────────────────────────────────────┐
│  agentwork-daemon（单进程）                               │
│                                                          │
│  HTTP API + WS hub  ──→  service 层  ──→  store(SQLite)   │
│        │                       │                          │
│        │                       │ bus.Publish（提交后）     │
│        ▼                       ▼                          │
│      WS fan-out            daemon 调度器                  │
│  (前端/agentwork-cli)       claim run → runTask           │
│                                  │  按 runtime.provider     │
│                                  ▼                         │
│                           runtime.Open(spec)             │
│                                  │  返回 transport 读写对   │
│                                  ▼                         │
│                           backend.Execute(Session)        │
│                                  │  acp|jsonl|jsonrpc       │
│                                  ▼                         │
│                           agent CLI 子进程                │
│                                  │ 回调 agentwork-cli       │
│                                  ▼  (+env: SERVER_URL…      )
└──────────────────────────────────┼─────────────────────-─┘
                                   ▼
                          agentwork-cli（agent 侧工具，CLI-as-tool）
```

### 分层职责

| 层 | 包 | 负责 | 不做 |
|---|---|---|---|
| **store** | `internal/store` | SQLite + schema，手写 SQL | 业务逻辑 |
| **service** | `internal/service` | 业务规则 + 事件发布（Goal/Run/Agent/Squad/Schedule）+ **goal 层仲裁 `reconcileOnRunEnd`** | 不执行 agent、不调 LLM |
| **daemon** | `internal/daemon` | run dispatch/调度、runTask、idle watchdog；**run 终态时调 goal 层仲裁，不直接写状态** | 业务决策、状态权威 |
| **runtime** | `internal/runtime` | transport 层：按 runtime spec 建连返回 backend 输入读写对 | 不讲协议 |
| **proto** | `internal/proto` | 协议层：多个 backend（acp / jsonl / jsonrpc），统一 `Backend.Execute → {Events, Result}` 抽象 | 不讲 transport |
| **server** | `internal/server` | HTTP + WS 边界，CORS | |
| **events** | `internal/events` | 进程内 pub/sub（同步、panic recovery） | 不持久化、不跨节点 |
| **cli** | `cmd/agentwork-cli` | agent 侧回调工具 | |
| - | `cmd/agentwork-daemon` | main：装配并启 HTTP+daemon | |

### 二进制

- `agentwork-daemon` — HTTP server + 内嵌 daemon。单进程。
- `agentwork-cli` — agent 侧工具。daemon 注入 agent 子进程 PATH + `AGENTWORK_SERVER_URL` / `AGENTWORK_GOAL_ID`（产品面 id）/ `AGENTWORK_RUN_ID`（执行面 id）/ `AGENTWORK_AGENT_ID`。让 agent 回调产生结构化副作用。

---

## 4. 数据模型

### 两张主状态表（双层中介）

```sql
-- goal：工作条目，产品面。状态权威的唯一持有者。
goal (
  id, title, description,
  parent_id            → goal.id,        -- sub-goal
  assignee_type        TEXT,            -- agent | squad | human
  assignee_id          TEXT,            -- agent.id | squad.id | ''(human)
  status              TEXT,            -- backlog | active | done | failed | blocked | cancelled
  handoff_note        TEXT,
  created_by_type     TEXT,            -- human | agent
  created_by_id       TEXT,
  created_at          TEXT
)
  -- backlog 是语义不变量：指派进 backlog 不启 run。
  -- parent_id 实现 sub-goal（不是 sub-run）。

-- run：一次执行，执行面。无权威，终态只向 goal 层汇报。
run (
  id,
  goal_id             → goal.id,
  agent_id            → agent.id,
  session_id          TEXT,            -- 协议层返回的 session id（历史/恢复）
  workdir             TEXT,
  status              TEXT,            -- queued | running | completed | failed | cancelled
  attempt             INT,
  result_summary      TEXT,
  trigger_comment_id  → comment.id,    -- 由哪条 comment 触发（mention / child-done）
  is_leader_run       INT,             -- 是否 squad leader 的 run
  squad_id            → squad.id,     -- 该 run 所属 squad（leader run 才有）
  queued_at, started_at, finished_at, created_at
)
```

**两个状态机，互不拉扯：**
```
goal.status（产品面——这活在哪？）        run.status（执行面——agent 在跑吗？）
backlog → active → done | failed          queued → running → completed | failed
        ↘ blocked                        ↘ cancelled
        ↘ cancelled
```

**backlog 语义不变量**：把 agent 指派进 backlog **不启动 run**；只有 `backlog → active` 或在 active 里建/指派才建 run。

### 其他表

```sql
runtime (id, name, provider, executable, args, endpoint, transport, env, created_at)
  -- provider ∈ acp|jsonl|jsonrpc：决定挂哪个 backend

agent (id, name, description, runtime_id, system_prompt, model, workdir_base,
       env, max_concurrent, created_at)
  -- 无 status/pid 死字段（per-task 模型下无意义；长驻模型才需要，留待未来）

squad (id, name, description, leader_id → agent.id, instructions, created_at)
  -- 路由组不跑活，活总走 leader。leader 必须是 agent（不允许 squad 当 leader）。

squad_member (id, squad_id → squad.id, member_type ∈ {agent, human}, member_id, role, created_at,
              UNIQUE(squad_id, member_type, member_id))
  -- 多态成员。不嵌套（member_type 无 'squad'）。

comment (id, goal_id, author_type ∈ {human, agent, system}, author_id,
         parent_id → comment.id,   -- 一层嵌套足够；文本树深度可未来扩
         content TEXT,              -- Markdown，含结构化 mention URI
         created_at)

chat_message (id, run_id → run.id, role, content, tool_calls, created_at)
  -- run 的输出流缓存（task detail 展示用）；按 run 归属，不是 goal

activity_log (id, goal_id, actor_type, actor_id, action, detail, created_at)
  -- 审计：handoff / child_created / children_done / squad_leader_evaluated / cancelled …

schedule (id, name, title_template, description, assignee_id, cron_expression,
          timezone, enabled, next_run_at, last_run_at, created_at)
  -- 定时触发：到点克隆一个新 goal + run，幂等靠 schedule_run 唯一索引
schedule_run (id, schedule_id, goal_id, planned_at, status, created_at,
             UNIQUE(schedule_id, planned_at))
```

`session` 表暂不加（per-task 模型每次新连接；长驻模型才需要，留待未来）。

---

## 5. 协调原语

四种"移交/推进"机制。均异步、goal 中介、队列驱动。

### 5.1 重指派（assignee handoff）

改 `goal.assignee` A→B（agentwork-cli `goal assign`）：
1. goal 层 UPDATE assignee + 存 activity_log(handoff) + 设 handoff_note（如有）。
2. 给 B 建 run（pending）。**不取消 A 在跑的 run。**
3. A 的 run 跑完时走 goal 层仲裁：goal 层见 `run.agent != goal.assignee` → **丢弃结果（不改 goal 状态）**，仅存为历史。

旧 run 中止与否不影响正确性：**改 goal 的 owner 是 goal 层操作；在跑的 run 是上一任的尾巴，其结果不拥有改 goal 状态的资格。**

### 5.2 子 goal 等待（fan-out + wait-children）

1. 父 run 的 agent 调 CLI `goal create --parent=<parent>` 建若干子 goal。
2. 父 run 的 agent 调 CLI `goal wait` → 父 goal 进入 `blocked`（等待集）。
3. 父 run 结束（这一轮完了，因为父 goal 被挂起）。
4. 每个子 goal 独立走正常调度链（各自被指派→建 run→执行）。
5. 子 goal 到终态时，触发**唤醒检查**：父 goal 是 `blocked`？所有非终态子 goal 都到终态了？（一个事务查，防竞态）。是 → 给父 goal 的当前 assignee 建新 run，附子结果摘要作 handoff_note。
6. 父 goal 第二轮 run，看到摘要，继续。

**等待集是动态的**：调 wait 之后新建的子 goal 也算。唤醒时检查"当前所有非终态子 goal"，不是调 wait 时的快照。唤醒靠"每 goal+agent 至多一 pending run"幂等（避免两个子并行完成重复唤醒父）。

### 5.3 @mention comment 触发

agent 在某 goal 下留言并 `@另一个 agent`，触发新执行：
1. agent 调 CLI `goal comment <goal> --content "[@Codex](mention://agent/<uuid>) ..."`。
2. comment 落库后，server 解析 comment body（**只扫已落库的 comment，不扫 agent stdout**）。
3. `mention://agent/<uuid>` → 给**被提及的 agent** 建新 run（**同 goal_id、不同 agent_id**），不取消在跑 run。`trigger_comment_id` 记录来源。
4. `mention://squad/<uuid>` → 路由到该 squad 的 **leader**，给 leader 建 leader run。
5. `mention://human/<uuid>` → 只渲染链接，不建 run（人不是执行单元）。
6. `@all`（`mention://all/all`）→ **抑制自动触发**（不建任何 run），仅通知人类。

mention 是结构化 Markdown URI（`[@Name](mention://agent/<uuid>)`），**只认 UUID，不认 `@handle` 散文**——agent 必须先 `agentwork-cli agent list` 查 UUID 再写结构化链接。这是"agent 主动调 CLI 产生结构化副作用"原则的具体落地。

### 5.4 squad 路由组

把多个 agent 组成队伍，路由到 leader，leader 按能力委派子 goal：
- 指派 goal 给 squad / @mention squad → 只给 **leader** 建 run（leader run），无 member fan-out。避免 N 个 member 同时被唤醒。
- leader run 的开场 prompt 注入 **squad briefing** = Operating Protocol + Roster + Instructions。
  - Roster 行：`- <name> — <kind>[, role: "<role>"][ — skills: a, b] — [@Name](mention://agent/<uuid>)` —— leader 可按能力委派而非靠 role label 猜。
- leader 委派 = 5.1/5.2 机制：指派子 goal 给 member agent，递归下去。
- leader 用 `agentwork-cli squad activity <goal> action|no_action|failed --reason "..."` 记录评估（进 activity_log，作 timeline）。

**status authority 单独门控**：briefing 注入只看 `is_leader_run`；但 leader 能否改父 goal 状态，另需 `goal.assignee_type=="squad" && assignee_id==squad.id`。被 @mention 进**别人拥有的 goal** 的 squad 是 guest，briefing 带禁令"不要改这个 goal 状态"，只有 owns 这条 goal 的 leader 才有推到 `done`/`active` 的权威。

**child-done 唤醒**：子 goal 完成时唤醒**父 goal 的当前 assignee**——父指派给 agent → 直接唤醒该 agent；父指派给 squad → 唤醒 leader（给 leader 建 run）。无 invocation gate（leader 本就 owns 父 goal，鉴权在指派时已做；子完成的执行者无可解析的人类 originator，二次鉴权会对默认 private leader 误关）。靠"每 goal+agent 至多一 pending run"幂等。

---

## 6. backend 协议层（多协议）

把现有 `internal/acp` 升格成 backend 抽象，ACP 只是其一。

```go
package proto

// Backend 是一个协议实现。Execute 在 transport 上讲它的协议，流式发
// Events，发一个终态 Result，然后关闭两个 channel。
type Backend interface {
    Execute(ctx context.Context, spec ExecuteSpec) (*Run, error)
}

// Conn 是 runtime.Open 建好的已开 transport 连接。
type Conn struct {
    R      io.Reader       // agent→client
    W      io.Writer       // client→agent
    Close  func() error    // transport 清理
    Stderr io.Reader       // 仅 stdio transport 才有；其余为 nil
}

type ExecuteSpec struct {
    Conn   Conn    // runtime.Open 建好的 transport 连接
    Cwd    string  // 该 run 的工作目录
    Prompt string  // service 已构建好的开场 prompt
}

type Run struct {
    Events <-chan Event            // 流式事件（Message/Thought/ToolUse/ToolResult/...）
    Result <-chan Result            // 恰好一个终态，然后关闭
}

type Result struct {
    Status     Status               // completed | failed | aborted | ...
    Output     string
    SessionID  string               // 供长驻模型复用
}
```

三个 backend：
- **`proto/acp`** — 包现有 `internal/acp`（ACP v1 JSON-RPC）。优先实现，其余后补。
- **`proto/jsonl`** — claude/opencode 风格的 `--output-format json` 单向 JSONL 流。turn 完成靠 `step_start`/`step_finish` 结构化配对推断；EOF 时仍处开 step 则 **fail-closed**（"stream ended without terminal signal"），绝不误判成完成。
- **`proto/jsonrpc`** — codex `app-server` 风格的双向 JSON-RPC 2.0，有显式 `turn/completed` notification，配 server→client notification 去重（completedTurnIDs）。

runtime 表 `provider` 字段决定拉哪个 backend。加新协议 = 加 backend，不动 daemon/调度层。

---

## 7. daemon 调度

### per-agent worker + 信号量

每个 agent 一个 worker，信号量容量 = `max_concurrent`。一个 agent 的 run 并行到其上限，不同 agent 互独立。

### claim 改进（修现有 head-of-line blocking）

现有全局 `LIMIT 1` 队列导致一个饱和 agent 把所有别的 agent 的 goal 全卡住。改为：

**claim 不全局抢一行，而是 per-agent 拉取**：daemon 维护每 agent worker 的空闲槽位，为有空闲槽位的 agent 各自认领它的 queued run。饱和 agent 不再阻塞别的 agent。具体实现（按 agent 分别 `UPDATE…RETURNING LIMIT slots`，或 worker 主动拉自己的队列）在实现阶段定。

### idle watchdog

纯静默达到 `window`（默认较短）则取消 run；**tool 在飞时切更大的 `toolWindow`**——`inFlightTools` atomic 计数，`tool_use` 无匹配 `tool_result` 时增，有匹配时减。有输出就永不杀，只限静默时长。现有代码定义了 `idleToolWindow` 常量却没用，属实现缺失，从 0 设计要落地这块。

### run 终态 → goal 层仲裁

run 完成（Result 到达）时，daemon **不直接 SQL UPDATE goal 状态**，而是调 `service.GoalService.reconcileOnRunEnd(run)`：

```
reconcileOnRunEnd(run):
  读 goal 当前状态 + assignee
  if run.agent != goal.assignee:                    # 已转交 / 重指派后
      存历史，不动 goal 状态，return                   # A 的尾巴不污染 B 的 goal
  if goal.status == cancelled:                       # 已取消
      存历史，不动，return
  switch run.status:
    completed:
      if 有未终态子 goal: 不动（等子完成流程）
      else: goal → done，并触发唤醒父 goal
    failed:
      if attempt 未耗尽: 建 run(attempt+1)
      else: goal → failed
  发 goal:finished 事件
```

**这是 agentwork 状态权威的内化点。** 现有 `finishTask` 直接 SQL `UPDATE task SET status=…` 的做法是 handoff/cancel bug 的根因，从 0 设计代之以 goal 层仲裁，根治。

---

## 8. 扩展点

- **新 HTTP 路由** — `handler.Mount` 加一行 + service 方法。service 在 `server.ListenAndServe` 装配。
- **新 transport** — `runtime.Open` 加 case。
- **新协议 backend** — `internal/proto` 加一个 backend 实现，runtime 表加 provider 值。
- **新事件类型** — `bus.Publish` + 加进 `ws/hub.go` 的 topics 白名单。
- **外部触发 → goal**（webhook、issue 轮询）—— 调 `GoalService.Create`(status=active) + 指派，复用同一条 dispatch 链。schedule 是现成先例。幂等靠外部事件 id 的唯一约束。
- **长驻 agent 模型**（未来，架构级）—— agent 创建时拉长驻进程，run 复用 `session_id`（ACP `session/load`/`resume`）。届时 `session` 表和 agent 活化字段才启用。当前 per-task 模型每次新连接。

### 不是扩展点（要改结构）

- **多节点** — `events.Bus` 进程内，无跨节点。单 daemon + 单 DB。多节点要外部消息总线。
- **多用户 / 鉴权** — 不做。单用户。

---

## 9. 工程不变量（让人机混合保持正确的硬规则）

1. **run 无权威，goal 层唯一权威** — 任何影响 goal 状态的写入走 `reconcileOnRunEnd`；run 直接 SQL 改 goal 状态是 bug。
2. **状态机解耦** — goal 执行面 / 产品面状态机互不拉扯；重指派、cancel、子等待只动 goal。
3. **不解析 agent stdout 提取意图** — agent 主动调 CLI 产生结构化副作用；mention 是结构化 Markdown URI（只认 UUID），server 只扫已落库 comment body。
4. **backlog 语义不变量** — 指派进 backlog 不启 run。
5. **每 goal+agent 至多一个 pending run** — coalescing safety net（partial unique index 或 service 检查）。避免重复 handoff/mention/child-done 反复入队重复 run。
6. **squad 不跑活** — 路由组只路由到 leader，无 member fan-out；status authority 单独门控（owns 才能改状态）。
7. **per-task workdir 隔离** — 每个 run 一个 `{workdir_base}/{run_id}`（或按 (agent,goal) 复用，长驻模型启用时）。

---

## 10. 与multica实现的取舍

| multica 的点 | agentwork 的取舍 | 理由 |
|---|---|---|
| server/daemon 分裂 + PostgreSQL | 单进程 + SQLite | 单机单用户，无外部权威需求；SQLite 即足够 |
| workspace / auth / 多租户 | 全去 | 单用户 |
| server 做 run 终态状态权威 | **goal 状态机内化权威** | 无外部 server，权威必须内化才能自洽 |
| 重指派不取消旧 run、两者并行（MUL-4113） | 同样不取消旧 run，但靠 goal 仲裁丢弃旧结果 —— 正确性不依赖旧 run | 消化历史包袱：无需外部权威也能自洽 |
| 每 provider 一个 backend（17 种各适配原生协议） | backend 抽象 + 三个协议族（acp/jsonl/jsonrpc），ACP 优先 | 单机不必一开始就上 17 个 backend；接口留好 |
| 全局 daemon 级并发池 | per-agent 信号量 | agent=人设+资源配额，per-agent 更贴合 |
| run 直接写 issue 状态 | **StartRun/CompleteRun 不碰 goal 状态** | 从设计层落实而非靠注释约束 |
| skill 系统 | 暂不做（agent system_prompt 已可承载 SOP） | 单用户 MVP 可后加 |
| inbox / 通知系统 | 暂不动（@all 的"通知人"在单用户下退化） | 可后加 |
| 28 张表 | 约 11 张表 | 去 workspace/auth/team/chat/inbox/notification/project/label/dependency 等多租户与协作面 |
