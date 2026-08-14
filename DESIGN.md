# agentwork — 产品与架构设计（冻结版）

> 本文档是 agentwork 的产品定义与架构基线，是实现的唯一依据。代码中的设计引用（`DESIGN.md §N` / `决策 N-M`）一律指向本文档。
> 覆盖：产品定义 / 闭环逻辑 / 核心概念 / 状态机 / 验收策略 / 工作区模型 / 交付 / 执行 / 工程不变量 / 模块边界 / 里程碑 / 决策记录。
> **变更纪律：先改文档、后改代码。实现与本文档冲突时以本文档为准，并修正实现或本文档。**

---

## 0. 产品定义（一句话）

> **agentwork 是"AI 干活的流水线操作系统"——全自动闭环运行 AI 任务，验收策略由人定义、机器执行、人只在卡点出现。系统管活，人管门。**

- **谁**：单机个人开发者（架构已焊死单用户，先个人后团队）
- **卖什么**：把 agent 从"互动工具"变成"无人值守设施"——任务在你离开时继续，回来时等你审
- **不做什么**：不做执行引擎（交给现有 CLI agent）、不做无人工全自动、不做通用工作流、暂不做多用户

**市场依据**：业界共识是 merge 是普世人门（Devin/Jules/Copilot/Codex 全部在 merge 前留人）；人审容量是瓶颈（与 AI 产出成正比增长）。现有产品的审核是固定的、粗的（plan/merge 两个 checkbox）。agentwork 的差异化：**卡点是第一公民**——可编程、可挂任意状态转换、可自我学习收敛。

---

## 1. 闭环逻辑

```
① 触发 → ② 执行 → ③ 机器验证 → ④ 验收判定 → ⑤ 卡点 → ⑥ 交付 → ⑦ 汇报
cron/Web  goal→run→   (域策略      (仲裁事务内)  review    合入/merge  Web
(IM/Git   agent 干活  自动跑)         │          (等人)      (平台动作)  (IM 未来)
为未来)       │              │                 │
     │              └ 失败→failed→重试→耗尽报人 ┘
     └──── 全自动段（人不在场）──────┘
```

注：① 触发渠道现状——cron / Web / issue 双通道（轮询+webhook）/ IM 入站（飞书），见下表；⑦ 汇报：IM 最小通知（goal 完成/卡点待审/失败推送）+ 审批卡 + 日报。

**人只出现在 ⑤ 卡点**，其余全自动。

### 触发渠道

| 渠道 | 机制 | 幂等 / 防重 |
|---|---|---|
| cron（schedule） | 模板 → 到点克隆 goal + 入队 | `uq(schedule_id, planned_at)` 一次触发一条 |
| Web | 建 goal / 指派 / 评论 mention | — |
| Issue（github / gitcode） | **轮询（默认 30s）+ webhook 双通道** → `CreateGoalForIssue` | `source_ref` 唯一约束——webhook 与轮询并发竞争时冲突被当作幂等结果；deliver 成功后 close + 结构化 commit 链接评论 |
| IM 入站（飞书） | 文本消息 → processor run（intake）解析意图 → 执行动作 | 消息 ID 去重；多域任务草稿澄清（10min 过期） |

### 自主度光谱（一个旋钮，不是三种模式）

**自主度 = 卡点密度，连续可调**（决策 2-2）：

| 旋钮位置 | 机器验证 | 卡点 | 人做的事 |
|---|---|---|---|
| 全自动端 | 强（CI/测试） | 无或极低风险 | 只看汇报 |
| 中间 | 强 | merge 必审 | 点按钮 |
| 对话驱动端 | 弱/无 | 每步都问（计划/阶段/收尾各挂卡点） | 全程指挥 |

**对话驱动不是独立模式**——它是同一个机制（review 态 + 唤醒 + 决策 note）的高频卡点配置。卡点可挂在任意状态转换点，密度由域策略决定。M0-M3 只做"旋钮"两端到中间的常用位置，最高密度端留待未来按需配置。

**规则：验证强度决定自主度**（见 §5.4——弱验证域强制提高卡点密度，防假绿失控）。

---

## 2. 核心概念模型

| 概念 | 定义 | 权威 |
|---|---|---|
| **domain**（域） | 资产/演进域 = 共享仓 + 验收策略 + 默认卡点。第一个实例：agentwork 仓 | 策略属域 |
| **goal** | 工作条目（产品面），状态权威唯一持有者。新增 review 态 | **唯一权威** |
| **run** | 一次执行（执行面），无权威，终态向 goal 层汇报 | 无 |
| **agent** | 人设+资源，**永不定义验证标准** | 无 |
| **runtime** | 进程形态（怎么连、讲什么协议），不含业务能力 | 无 |
| **卡点** | 验收策略中"必须停给人判定"的规则 | — |
| **验收策略** | 完成判定的完整定义：机器验证 + 结构化约束 + 卡点规则 | 属域 |
| **mention** | 协作原语 = **Consult（问）的唯一协议语义**（决策 5-1/5-2）——评论中的结构化 URI `[@Name](mention://agent\|squad\|human\|all/<id\|all>)` 触发 guest run（只读，决策 5-3），不改变 goal owner、不创建子 goal；平台解析已持久化的评论体，绝不解析 agent stdout | 评论属 goal |
| **squad** | 路由组，不干活——分配给 squad / @squad 只路由到 leader；成员多态（agent\|human）+ role，`role=reviewer` 成员被平台自动拉入审查 checkpoint | 无 |
| **sub_goal** | 拆出的工作项实体（决策 6-1）——**不是 child goal**：不可递归、依附 goal 生命周期、无独立交付；assignee（做）+ verifier（验）分离，两层失败计数 | goal |
| **change** | 跨 workspace 的逻辑交付物（决策 6-3）：4 态（ready/integrating/integrated/conflict），Revision 绑定 Integration Base；与第一版 Revision 同事务原子创建 | goal |
| **verification** | sub-goal 的质量判定轮次（决策 6-5）：机器（域验证命令）或 agent verifier 的结构化 verdict，verification_result 审计 | goal |
| **attention** | OwnerAttention 派生状态（决策 6-8）：integration/recovery/user_action，Coordinator 事务内持久化到 `goal.attention`——UI 徽标与 IM 通知去重依据 | goal |
| **processor agent** | 平台侧 LLM 执行者（NL→checks 编译、验证强度推断、IM 入站解析）；与 worker 同级配置，run_kind=processor，产物走文件（文件即副作用） | 无 |

---

## 3. 数据模型

```
+ domain 表:
    id, type(repo), name,
    git_url, default_branch, git_identity(用户/邮箱), git_credentials,
    policy_text(TEXT, NL 意图源真相), checks(JSON, 编译产物),
    verification_strength(强/中/弱),    -- LLM 推断（决策 2-5），用户确认后与产物一起冻结
    max_run_duration(默认 2h), verify_timeout(默认 10min),
    checks_compiled_at, metrics_baseline(JSON, 演进指标基线——测试数/覆盖率，M0 建域时记录),
    created_at
    -- 处理器 agent（processor_agent_id）：全局配置默认，域可覆盖（决策 2-17）
    -- M0 仅实现 type=repo；其他资产域类型（文档/配置/知识库/backlog）后置（§13）
+ goal 表新增:
    domain_id → domain.id（agent 执行的 goal 必须有域）
    review_request（卡点触发原因/证据包指针）
    human_iterations INT（人 reject 迭代计数，与 run.attempt 分开——盲点 2）
    execution_attempt INT（决策 6-9：机器重试计数——见决策 6-9 注记：goal 层重试目前仍由 run.attempt 承担，本列建而未用）
    attention TEXT（决策 6-8：OwnerAttention 派生状态持久化——'' | integration | recovery | user_action）
    -- worktree_path / branch_name 已退役（决策 6-2：worktree 按 run 实例化；goal 分支名由 goalID 派生 feat-<前8位>）
+ run 表新增:
    run_kind(worker/processor——处理器 agent 的内部 run 与 worker run 同一套调度机制，决策 2-17)
    run_type(compile|intake)（processor 子任务分类，M3 扩展：摘要/健康分析）
    gates_hit TEXT(JSON)（门禁命中记录——daemon 计算，goal 层仲裁判定，append 到 run 行）
    trigger_comment_id（触发源评论——mention 溯源 / guest run 失败留痕 / pingpong 计数口径）
    is_leader_run / squad_id（squad leader run 标记；权威判定按 reconcile 时刻动态查表，不信任静态标记）
    evidence TEXT(JSON)（证据包：diff 统计 + 验证输出 + agent 汇报摘要；卡点触发时生成，审批卡引用）
    role（决策 5-4/6-9：owner|subgoal|consult|review|verify——enqueue 时派生的信息快照；归属判定仍按 reconcile 时刻动态查表）
    sub_goal_id / base_ref / head_ref（决策 6-1/6-3/6-9：sub-goal run 的工作项指针 + Change 修订的 base/head ref——sub_goal 与 run 是 1:N，关系真相 = run.sub_goal_id，无 current_run_id 指针）
    dirty_snapshot（已退役——决策 6-2 run 级 workspace 后仅保留列）
+ v2 协作实体表（决策 6-1/6-3/6-5/5-8/5-9）：
    sub_goal（工作项：assignee+verifier、status 8 态、execution_attempt/quality_iteration）
    change / change_revision（逻辑交付物 + 修订；Change 与第一版 Revision 同事务原子创建，Ready ⇔ 已持久化 Revision）
    verification_result（每轮验证一条：verifier_run_id、passed|rejected、summary+evidence；轮次 ≠ run.attempt）
    handoff_event（所有权转移 append-only 审计：from/to 多态 + from_run_id/to_run_id + reason + actor）
    consult_request（consult 全链路：requester_run_id 即恢复锚点、trigger/guest/response 三评论）
+ gate_decision 表（卡点决策留痕 + 健康度学习数据源）:
    id, goal_id, run_id(证据包关联), gate_rule, decision(approve/reject/redirect),
    reason, decided_by, decided_at, review_duration
- agent 表删除: workdir_base（域决定在哪干活；无域 = 纯 backlog 人活）
- 迁移机制: M0/MVP 不做（数据可弃，wipe 重建）；M1 引入 schema_version + 顺序脚本
```

---

## 4. 状态机设计

```
goal:  backlog → active → done | failed | cancelled
                  ↘ review（等人类决策）
                  review → done（approve + deliver 成功）
                  review → active（reject → 新 run 带决策 note；或 deliver 失败后人工决定）
run:   queued → running → completed | failed | cancelled（handoff 语义由 cancel_reason=handed_off 承担，决策 6-6）
```

### 关键时序（验收判定在仲裁事务内）

```
agent 干完（run 到终态）
→ daemon 跑域验证策略（事务外：go test 等耗时命令 + 结构化约束检查）
→ 验证结果落 run
→ 触发 ReconcileOnRunEnd（事务内）：
    读验证结果 + 卡点规则
    → 全过且无卡点命中 → done
    → 验证挂 → run failed → 重试（≤3：sub-goal 层 execution_attempt 为权威；goal 层目前由 run.attempt 承担，见决策 6-9 注记）→ 耗尽 → failed（报人）
    → 卡点命中 → review（等人）
    （sub-goal/verify run 路由到 ReconcileSubGoalRun / ReconcileVerifyRun，不碰 goal 状态——决策 6-1/6-5）
→ 人 approve/reject → ResolveReview → approve 触发 deliver（§7）/ reject 回 active 带决策 note（新 run 从分支 HEAD 切新 workspace，决策 6-2）
```

### 规则

- **per-workspace 串行化（决策 6-2）**：每个 run 有独立 worktree；owner run 单飞 + 每 sub-goal 至多一个活跃 run，consult/review/verify 只读可与 owner 并行。service 层 enqueue 检查 + daemon 调度层保证，不做 DB 约束。
- **attempt 分开计数**：机器失败重试（`run.attempt`，上限 3）与人类 reject 迭代（`goal.human_iterations`，不设硬上限）分离，合法迭代不得被误掐断。reject 的触发源记入 `trigger_comment_id`（已有字段）。
- **maxRunDuration（决策 2-6）**：单 run 最大时长（默认 2h，域策略可调），超时 → run 记 **cancelled（不消耗 attempt）**，goal 保持 active，汇报人"任务超时"——人决定重开或放弃（重开 = 新 run 从分支 HEAD 切新 workspace，决策 6-2）。与 idle watchdog（静默）互补。
- **verify_timeout**：验证命令自身超时（默认 10min，域策略可调），超时视同验证失败（防挂死的 verify 让 run 永远 running）。
- **review 冻结的是 execution 不是 intent（决策 2-3，2026-08 修订）**：goal 在 review 时，mention/handoff 等协作动作**照常产生 run，但 run 只能停在 queued**——Claim 的 goal 状态门（决策 6-15）拒绝 claim（唯一豁免 role=review 的平台审查 run）。审批针对的分支状态不被污染（没人执行），执行意图也不丢失。reject → active → queued 意图自动 claim；approve → deliver 成功 → goal done，queued 意图被 MarkDelivered 取消（可见地留在 run 历史）。原语义"只落 comment 不触发 run"作废。
- **worktree 干净性（决策 6-2 修订）**：run 级 worktree 使脏检查退役——runs/<runID> 路径即归属，脏的就是该 run 自己的产物（crash 恢复复用）；consult/review/verify 结束时平台 reset --hard + clean 丢弃一切改动。
- **cancel/delete 语义**：Cancel 覆盖 review 态；delete 时级联清理 worktree（并入 worktree 生命周期）。
- **人工停止 run（决策 4-12）**：`POST /goals/{id}/runs/{runId}/stop` 终止 running run（reason_code=`stopped`）——执行层操作，**goal 状态不动**（run 无权威）；不耗 attempt、不自动重试、notify 不推"任务中断"（人自己停的）。恢复由人决定：worktree 文件态保留（决策 2-6/6.2 恢复模型），重新触发 / 换 agent / 评论引导。**Cancel 增强**：Cancel goal 时同时终止 running run（同一机制）——已判死的 goal 不再白烧 compute。stdio transport 的 Close 修复（进程组杀 + 关读端）保证停止即刻生效（agent 子进程不再持有 pipe 写端卡死 run）。
- **handoff 终止旧 run（决策 4-9，协作防死锁）**：goal 易主（人改派 / agent handoff）时，平台终止旧 owner 的 running run——否则旧 agent 认为"已交接"而不结束 run（继续干是白干、空等被 watchdog 打死、活跃轮询则 watchdogs 都拦不住），新 owner 的 run 被 per-goal 串行化永远堵在 queued：死锁。机制：daemon 维护 runID→prompt cancel 注册表（+ 取消原因码），`goal:assigned` 事件驱动取消非新 owner 的 running run；claim→注册窗口内的 run 由 DB 兜底标记（status→handed_off，决策 5-5），runTask 注册后自查 self-cancel（窗口归零）；旧 run 终态 cancelled + cancel_reason=handed_off（不耗 attempt，不触发自动重试；决策 6-6 撤销 5-5 的独立状态）；新 owner 从 branch HEAD 切新 workspace，不代提交 WIP。重新指派给同一 agent 不终止（它仍是 owner）；assign 给 human 终止所有 running run。AGENTWORK.md 同步引导"交接后立即结束你的 turn"。
- **取消原因结构化（决策 4-9 延伸）**：run 行 `cancel_reason` 列 + 事件 `reason_code`（`idle_watchdog|handoff|stopped|timeout|runaway`）——语义判断用结构化码，不用字符串匹配（result_summary 的 "idle watchdog: " 前缀仅为展示）。效果：maxRunDuration 超时不再被误判为 watchdog 自动重试（决策 2-6：超时不重试，人决定）；notify 对 handoff/stopped 取消不推"任务中断"卡；`runaway`（决策 6-15 的 runaway reaper）推卡——run 越界被杀必须让 owner 知道。
- **agent 无权请求审批（决策 4-11）**：审批触发 = 机器 gate（域策略卡点规则）+ 人——agent 的 `goal request-approval`（M2 行为卡点实现）已移除：执行者不得请求判定（三角分离不变量 9）——"什么时候要人审"由卡点规则表达（如 merge 必审），不是 agent 自觉（不变量 10）。agent 干完活 → run 自然 completed → 机器判定（gate 命中 → review；无卡点 + 验证过 → done）；squad 审查 run 在 review 窗口自动触发（平台机制），意见进评论区供审批参考——审批对象只有一个（触发 review 的 worker run），reviewer 意见不是审批对象。
- **协作四行为模型（决策 5-1/5-2，替代 4-9 的接力文案）**："在 run 内等对方结果"物理不可能——owner run 单飞 + 每 sub-goal 至多一个活跃 run（consult/review/verify 只读可与 owner 并行，决策 6-2）。协作只有四种行为：Comment（说——纯评论，mention 永不触发 run）、Consult（问——mention 触发只读 guest run，完成后平台自动恢复 requester，决策 5-8）、Handoff（接力——所有权转移，旧 owner run 终止（cancel_reason=handed_off，决策 6-6）+ 新 owner run，决策 5-6 权限）、Sub-goal（拆活——sub_goal 实体，不是新 goal，决策 6-1）。依赖对方信息 → consult；依赖对方干活 → handoff 或 sub-goal。AGENTWORK.md 完整引导。
- **评论即重开（决策 4-1）**：终态（done/failed/cancelled）+ **human 作者** + 评论含 **action mention**（agent/squad）→ 自动 Reopen 再触发；纯评论仅落库不重开——终态不接受静默新活。agent/system 评论不触发重开。
- **mention pingpong 上限（决策 4-2）**：计数口径 = `trigger_comment_id` 指向 **agent 作者**评论的 run 数。>4 → 下一 run 注入"停止互相转移任务"警告；≥8 → 强制 failed（协作循环判死，系统评论留痕）。human/system 触发不计。
- **guest run 失败留痕（决策 4-3）**：协作 run（非 assignee 触发）失败且带 `trigger_comment_id` → 写系统评论（"协作 run 失败：…"），不重试、不动 goal 状态——feed 可见即协作闭环。
- **squad 审查 checkpoint（决策 4-4）**：squad 拥有的 goal 进 review 时，平台自动给 `role=reviewer` 成员（排除 leader 自审）发系统 mention 并 enqueue 审查 run——审查是平台机制，不是 agent 自觉；审查 run 为 guest run，意见只活在评论里供审批参考。脏 worktree 停靠（平台问题而非完工）不触发审查。**每个 completed run 的汇报由平台落 feed（insertRunResultComment）**：汇报是 run 的交付记录（完整保留不截断），guest 与 assignee run 一律落——驳回/批准时的审批上下文必须完整；agent 的自觉评论是附加对话，两者不互斥。**补充（审批对象语义）**：
  - **被审批对象 = 触发卡点的 run（owner run）**——证据包/汇报引用它，不是"最新 completed"（审查 run 必然晚于触发 run 入队，时间近似会把审批对象错位成审查者——曾把审查 run 的失败/意见展示为被审批对象的证据）。面板取数规则：agent 目标取 `agent_id == assignee_id` 的最后一个 completed run；squad 目标取最后一个 `is_leader_run` 的 completed run。
  - **评论带 run 归属**（`comment.run_id`，run 产物落库）：审批面板的**审查意见区**只显示**本轮 review 窗口内**审查 run 的产出（非 owner run 且完成于触发 run 之后）——worker 汇报、历史轮次的审查意见不占本轮审批视线（它们在评论区全量可见）；证据区=触发 run 的机器证据 + worker 汇报；卡点原因=review_request。审查意见是参考观点，不是证据——证据 = 机器验证产物 + diff。
  - **审查 run 只读**：触发评论为 system 作者的 run（平台审查请求）结束时**不 commitRunChanges**——审查产物（若有，测试产物等）不得混入 goal 分支；"只提意见不改文件"从 prompt 指令升级为平台机制。**2026-08 修正（决策 5-3）**：残留改动不再走下一 run 的脏检查 park 报人——guest/review run 的窗口期改动由平台在 run 结束时直接丢弃（entry 快照求差）；guest 直接 commit 进分支的检测到写系统评论提醒。
- **评论区语义（决策 4-6）**：评论区是 goal 的**协作交流区**——只承载"人的话"与协作动作（创建指令、改派 handoff note、审批理由、重开理由、人/agent 对话、mention 触发），作者为 human / agent / 必要的系统提示。**系统内部状态不入评论**（run 终态、进入审批、合入、超时）——状态史由活动日志 + 执行流承载，评论不是日志。**评论 = 团队上下文的对话层**：每个 run 的 prompt 注入该 goal 的**完整评论 feed**（不限作者、不限条数）——被 mention 拉进来的 agent 必须看到别人对它说了什么，协作链不能断。
- **评论注入压缩（决策 4-7，规划未实现）**：全量注入的代价随评论增长，平台负责压缩（平台机制，不是 agent 自觉）——预算制分层：**保真层**（触发评论原文、approve/reject/handoff 等决策记录）永不进摘要；**保留层**（最近 ~4K 估算 token 的活跃对话）原文注入；**背景层**（更早评论）由平台维护的累积摘要承载。摘要由平台内部调度的 processor run（run_type=summary，专用 summarizer agent）生成，不占干活 agent 的并发槽；触发 = 未摘要窗口达压缩阈值（~8K 估算 token），每次把最老的压进累积摘要、回落到保留预算，一次处理量恒定。估算 token 用启发式（CJK×1.2 + 非CJK×0.28，纯平台代码，误差 ±30% 只影响触发时机，不破坏保真不变量）。失败：重试事件驱动（新评论到达才再试，连续 3 次失败后降级常驻），**摘要失败不影响 goal 状态**——降级为机械压缩（最近保留层原文 + 更早「作者: 首句」），纯平台代码永远可用；恢复 = 新评论到达重置计数。不设迁移/回填：开发期直接 wipe。
- **client 执行环境代理（决策 4-8，已实现）**：worktree 永远在 agentwork（client 侧），agent 经协议访问，不依赖共享文件系统。**双通道**：(1) **ACP fs/terminal RPC**（fs/read_text_file、fs/write_text_file、terminal/*——实现 runEnvironment 8 handler，握手声明 clientCapabilities，request_permission 自动放行——那是 client 的**工具许可**，不是平台的审批卡，信任边界 = daemon 用户）；(2) **MCP 工作区 server**（官方 go-sdk，streamable HTTP，/mcp/{runID} 挂 GET+POST——GET 是 SSE 订阅，缺失会 405 导致工具不注册；session/new 的 McpServers 注入 URL）——给工具不走 client RPC 的 agent（opencode 本地工具）的通道。**MCP 工具 = 异步 terminal 形态**（与 ACP terminal 同一 terminalManager，一个引擎两条通道）：`agentwork_read_file` / `agentwork_write_file`（fs）+ `agentwork_terminal_create`（启动命令返回 id，立即返回）/ `agentwork_terminal_output`（增量轮询 + 退出状态）/ `agentwork_terminal_release`（杀+清理）——同步 run_command 已退役（挂 HTTP 无超时、双实现）。terminal_create 可带 timeout（命令级超时杀进程组）。**统一模型**：所有 run（stdio/ws/tcp，**含 processor run**——曾漏配导致编译 agent 写不出 checks.json）都注册 handler、声明能力、注入 Workspace 指引段（环境事实 + 协作契约，点名平台自有工具，不指 agent 工具名）。**run 上下文**：terminal/create 注入 AGENTWORK_* + PATH（CLI 本机直连）；MCP executor 绑定 worktree + run env。**terminal 生命周期 = per-command**，清理 = session 关闭时平台统一 kill 遗留（MCP 命令同池子同清理）。**无路径限制**：信任边界 = daemon 用户权限。**遗留**：fs 读敏感文件（.ssh、平台凭据）的拦截未做；MCP URL 暴露 runID 即访问权（远程暴露时需 capability token）——均列后置。

### 数值守卫（状态机不变量，不是提示词请求）

| 数值 | 语义 |
|---|---|
| execution_attempt ≤ 3 | 机器失败重试上限：sub-goal 层为权威计数（决策 6-9）；goal 层目前由 run.attempt 承担（goal.execution_attempt 建而未用，见决策 6-9 注记） |
| mention &gt;4 / ≥8 | agent 间协作 run 数：注入"停止互相转任务"警告 / 强制 failed（决策 4-2）。**2026-08 修订（决策 6-15⑩）**：subgoal/verify 豁免（其 trigger 是 owner 的派发评论——workflow 执行不是 mention 循环）；当前口径是近似，最终语义 = interaction edge 非 role |
| handoff ≥4 / ≥8 | 所有权交接次数：系统评论警告 / goal 进 review 审停（决策 5-7，不判 failed） |
| deliverWaitForRuns = 5min | approve 后只等该 goal 的 **owner run**（sub-goal/consult/verify 不写 goal 分支，不阻塞交付——决策 6-6），超时报告"请稍后再批准" |
| verify_timeout = 600s | 单条验证命令超时（−1 视同失败，防挂死 verify 让 run 永远 running） |
| max_run_duration = 7200s | 单 run 最大时长，超时 → run cancelled（不耗 attempt），goal 保持 active |
| idle watchdog 2min / 10min | 无输出静默停摆 / tool 在飞放宽窗 |
| workerQueueDepth = 64 | per-agent 队列深度，满则 run 退回 queued（attempt 不动，下 tick 再 claim） |
| dispatchTick = 500ms | daemon 调度轮询间隔 |
| issue poll = 30s（下限 15s） | issue 轮询间隔（防限流）；配置 webhook 后事件即时到达，轮询兜底 |

---

## 5. 验收策略（一等概念，属域）

### 5.1 结构

```
验收策略 = 完成判定的完整定义
├── 环境准备（setup）：依赖安装等验证前置（npm install 之类，幂等；平台在干净 worktree 上执行，依赖不会自己存在）（决策 3-1）
├── 机器验证：命令列表（go test ./... 等，exit 0 = 过；失败自动重试 1 次排除 flaky）
├── 结构化约束：diff 必须命中 *_test.go、改动文件数 ≤ N、覆盖率提升 ≥ N%（M0 自举任务即用此约束——决策 2-16）、不得触碰 config/*（自动查，客观）
└── 卡点规则：merge 必审、动敏感路径必审、验证强度弱时强制人工段
```

### 5.2 三角分离（不变量）

| 角色 | 谁 | 铁律 |
|---|---|---|
| 定义 | 域所有者（用户） | 声明"这个仓怎么算对"——**用自然语言** |
| 执行 | agent | **永不定义验证标准**（裁判不能是运动员） |
| 判定 | 平台机器 + 人 | 机器按策略跑，人在卡点把关 |

### 5.3 声明分层与编译流程

```
平台安全基线（内置，不可覆盖）：动生产/花钱/删文件必审
    ↓
域策略（用户 NL 意图）："测试要过、别动生产配置、性能不能降"
    ↓ 编译（定义时刻，平台内嵌 LLM 调用——决策 2-4；独立于 worker，产物冻结）
检测技术栈 → 产出检查清单 + 卡点清单 + 验证强度推断（决策 2-5）
→ 弹确认卡 → 用户确认/修改 → 冻结（强度与产物一起冻结）
    ↓ 编译不出来的要求（"性能不能降"）→ 自动升级为卡点（人判定段）
    ↓
任务覆盖（goal 级 NL 要求，后置）：特殊任务的额外验收标准
```

- **用户接口是自然语言，命令是编译产物**。产物可见、可改、可审；worker 永远接触不到编译过程。
- **编译器 = 通用处理 agent（processor agent），与其他 agent 同级配置（决策 2-4 修正）**：平台自身不直接调任何 LLM API；所有平台侧 LLM 工作（NL 编译、强度推断，未来：transcript 摘要、汇报生成、健康度分析）都通过配置的处理器 agent 执行。处理器 agent 与 worker agent 一样是 runtime+人设配置，可复用同一执行工具（如 openagent-cli），用户可换。
- **结构化输出走文件，不解析 stdout**：处理器 agent 把编译产物写入其工作区文件（如 `checks.json`），平台读取——与"不解析 stdout、只认结构化副作用"的哲学一致（文件即副作用）。
- **用户确认不变**：产物弹确认卡，确认/修改后才冻结（`checks_compiled_at` 可审计）——这是"定义权归人"的最终保障。三角分离精化：worker（干活）≠ processor（编译）≠ 用户（定义）。
- **降级路径（决策 2-8）**：未配置处理器 agent 或 LLM 不可用时，允许手动输入验证命令（跳过编译），建域不阻塞。
- 产物冻结后，所有任务的验证都跑这份清单。

### 5.4 验证强度 → 卡点密度联动

域策略声明验证强度——**由处理器 agent 在编译时一并推断（决策 2-5），只做默认值**：
- 推断结果展示在确认卡上，用户可手改；确认后与产物一起冻结，可审计
- 用 LLM 而非 heuristic 的理由：能理解任意自定义命令的语义（`check-links.sh` 也能判断它是链接验证）；且与编译由同一处理器 agent 在同一 run 内完成，不增加额外执行

强度 → 卡点密度规则：
- 强验证域 → 自动段可长，默认卡点少（信任机器判定）
- 弱验证域（echo ok 之类）→ 平台强制提高默认卡点密度（多用人工段）
- 这条把"自主度光谱"变成机制，防假绿失控。

---

## 6. 工作区模型

### 6.1 域持共享仓，goal 持 worktree

```
~/.agentwork/runs/
 ├── repos/<domainID>/         共享仓（bare，只 clone 一次；origin refspec 指 refs/remotes/origin/*）
 ├── runs/<runID>/             per-run 临时 worktree（决策 6-2：owner 检 feat-<goal> 分支；
 │                             sub-goal 分支 feat-<goal>-sg-<sub>；verify detached 快照）
 └── deliver-<goalID>/         交付临时 worktree（detached 于 origin/<default>，交付后移除）
```

- "在哪个域就在哪干活"：goal 挂 domain → daemon 为该域的 bare 共享仓分配 **run 级临时 worktree**（独立目录；owner 检 goal 分支，sub-goal 从 goal 分支 HEAD 切自己的分支，verify detached 只读快照）→ run 在此工作
- 多任务并行：每 run 一个 worktree 互不干扰（git worktree 是业界并行 agent 标准做法）
- **worktree 是暂态（决策 6-2）**：git 一个分支只能一个 checkout——run 结束立即释放（defer git worktree remove），下一个 run 从分支 HEAD 切新 workspace；checkpoint 走提交，不走文件态
- **延迟分配（决策 2-18）**：worktree 在 run claim 后才创建，backlog 挂起不占磁盘资源
- **共享仓同步**：每次 run 开始前 daemon fetch 共享仓（分支 base 新鲜）；deliver 时 fetch 主分支。**fetch 与 deliver 共用同一把 per-domain 锁（决策 2-10）**——并发 fetch 同一共享仓会 index.lock 冲突
- worktree 磁盘目录生命周期：终态 run 的目录保留 worktreeRetentionDays=7 天后由 cleanupWorktrees 清理；崩溃遗留由启动 sweepRunWorktrees（worktree prune + 清 runs/）与 sweepDeliverWorktrees 处理（未提交 WIP 丢失是接受的代价——恢复 = 提交态 + transcript）

### 6.2 上下文恢复（A5，CPU 中断模型）

| CPU 中断 | agentwork 卡点 |
|---|---|
| 保存现场（寄存器/PC） | run 事件持久化到 `chat_message` + 改动提交进 goal 分支（run 结束 commitRunChanges，checkpoint 走分支态） |
| 处理中断 | 人审批（review 态） |
| 恢复现场 | 新 workspace 从分支 HEAD 切出（决策 6-2 修订：分支态 checkpoint）+ 重放上轮 transcript（会话态） |
| 继续执行 | agent 从被打断处继续 |

- **M0/M1：swap 模型**——现场存在 SQLite 和磁盘，等多久都不怕（进程不保活，机器重启不丢）。
- **长驻路径**：ACP `session/load`/`session/resume`，`session_id` 已记录；等 agent 支持服务端会话时升级为进程级恢复。
- transcript 重放：只重放被中断那轮的 transcript；超阈值截断/摘要（摘要后置）。
- worktree 干净性不再是恢复前提：`runs/<runID>` 路径即归属（决策 6-2），脏的就是该 run 自己的产物；consult/review/verify 结束时平台 reset --hard + clean 丢弃一切改动（只读契约）。

---

## 7. 交付：自动合入

```
卡点 approve → daemon 在临时 deliver worktree 执行 deliver（确定性脚本，无 LLM）：
  等 owner run 结束（deliverWaitForRuns，决策 6-6）→ runs/deliver-<goalID> detached 检出
  origin/<default> → git fetch → git merge --no-ff <goal分支>
  → 合并后【再跑一遍域验证】（合并后的 main 必须绿）
  → git push → goal 层 MarkDelivered → done → 移除临时 worktree
失败（冲突/验证红）→ 不 push，goal 回 review 标注失败原因
  → 人可：重试 deliver / reject 回去让 agent 修（reject 语义天然覆盖）
```

- 合入是**平台动作不是 run**——不派 agent 去 merge（避免"干活的人自己验收自己"）
- 验证执行两处：run 结束 + 合入前（保证进主分支的代码是绿的）
- **同域 deliver 串行化**：同一 domain 的多个 deliver 排队执行（daemon per-domain 锁），避免多 goal 并行开发时并发 push main 冲突
- **deliver 幂等化（决策 2-9）**：daemon 崩溃重启后，发现"goal 在 review 且已 approve 但未 done"→ 重新执行 deliver；merge 前检查（分支已合 → skip）、push 前检查（已推 → skip）——半途崩溃不丢步骤
- git 凭据/身份入域配置（如 `agentwork[bot]`）
- **交付后回滚（决策 2-7，方向记录）**：远程 CI 集成（后置）时配套"CI 红 → 自动 revert"规则；远程 CI 之前没有客观失败信号，靠人发现后走 failed 回收路径
- 这一步是 repo 适配器的种子，M0 必做

---

## 8. 执行层

- **M0 执行工具：`openagent-cli serve --acp`**（`~/opensource/openagent-go/build/openagent-cli`，ACP over stdio）——与 acpbackend 协议连通性已验证。
- runtime 配置：transport=stdio, provider=acp, executable=openagent-cli 路径, args=["serve","--acp"]。
- **openagent-cli 只是执行工具**——不依赖它的任何特性（--approver/--channel/--summarizer 等一律不使用不响应）。见 §9 不变量。
- **处理器 run 形态（决策 2-17）**：处理器 agent 的活也是 run（`run_kind=processor`，挂 domain 不挂 goal），与 worker run 同一套调度机制；处理器 agent 建议独立配置（与 worker 共用同一 CLI 时，per-agent 信号量会让编译 run 排队等 worker run 完成——M0 单 agent 场景无碍）。
- 备选：claude（jsonl，需先实现 jsonlbackend）、opencode——后置。
- idle watchdog（已有）：静默 window 2min / tool 在飞 10min。

### 调度与并发

- **分发**：dispatchTick（500ms）轮询，只对有空闲并发槽的 agent 集合做 Claim；Claim 用单条 SQL 原子地把最老的 queued run 标为 running，`NOT EXISTS` 保证同 goal 至多一个 running（per-goal 串行，§4）——mention run 到达时**等**而不是抢（曾静默取消导致丢审查步骤）。
- **并发上限**：per-agent worker + 信号量（= `max_concurrent`）；不同 agent 完全独立并行。worker 队列满（64）→ run 退回 queued。
- **processor run**（goal_id=''）不受 per-goal 串行限制。

### 崩溃恢复（启动扫描）

daemon 启动时按顺序恢复，状态全在 SQLite + worktree，重启不丢：

1. `RecoverStuckRunning`——遗留 `running` run 全部复位 queued，重新 claim；
2. `recoverWorkers`——重建全部 agent worker；
3. `recoverPendingDelivers`——扫描"review 且已 approve 但未 done"的 goal，重放 deliver（merge/push 幂等保证安全，决策 2-9）；
4. `sweepRunWorktrees` + `sweepDeliverWorktrees`——崩溃遗留的 per-run 临时 worktree 与 deliver worktree（git worktree prune + 清目录，决策 6-2；恢复 = 提交态 + transcript，未提交 WIP 丢失是接受的代价）。

### agentwork-cli 命令面（跨 agent 协作通道）

daemon 把 CLI 目录注入子进程 PATH + `AGENTWORK_SERVER_URL/GOAL_ID/RUN_ID/AGENT_ID` 环境变量。命令：

```
goal list | assign | create | comment | wait
agent list · squad list · issue list
```

agent 通过它产生全部结构化副作用（mention / 审批请求 / 子任务创建），平台绝不解析 agent 输出流（不变量 3）。**2026-08 修正（决策 4-13）**：agent 协作已改为 MCP 工具，CLI 保留给 human 调试——agent 不再学命令语法。**2026-08 再修正（决策 5-2，替代 4-13）**：MCP 协作工具面重构为四行为模型——comment_goal（说）/ consult_agent（问）/ handoff_goal（接力）/ create_sub_goal（拆活）/ goal_wait（挂起等子完成）+ goal_list/agent_list/squad_list（查看）；goal_comment/goal_assign 移除。**2026-08 三修（决策 6-10）**：blocked/wait 机制退役，goal_wait 移除；工具面扩充为 cancel_sub_goal / verify_sub_goal / integrate_change / get_change / get_sub_goal / get_verification。

---

## 9. 工程不变量

1. **run 无权威，goal 层唯一权威**（任何影响 goal 状态的写入走仲裁）
2. **状态机解耦**（执行面/产品面互不拉扯）
3. **不解析 agent stdout 提取意图**（agent 主动调 CLI 产生结构化副作用；mention 是结构化 URI）
4. **backlog 语义不变量**（指派进 backlog 不启 run）
5. **每 goal+agent 至多一 pending run**（coalescing）
6. **squad 不跑活**（只路由到 leader，status authority 单独门控）
7. **worktree 模型**（域持共享仓，goal 持 worktree+分支——见 §6）
8. **验收判定在仲裁事务内**（卡点判定与状态推进原子，不建在事件订阅上）
9. **三角分离**：定义（域所有者）≠ 执行（worker agent）≠ 判定（平台机器+人）。**worker agent 永不定义验证标准**；NL→checks 编译由独立配置的处理器 agent（processor agent）完成，产物必须经用户确认后冻结——用户确认是"定义权归人"的最终保障
10. **平台能力不依赖任何特定 agent 特性**——agent 的唯一职责是执行，agentwork-cli 是唯一跨 agent 协作通道；卡点/验证/交付/通知/摘要全部平台自建
11. **验证与执行者无关**（同一任务谁干都按同一标准验）
12. **per-workspace 串行化**（决策 6-2 修订：worktree 按 run 实例化不可并发写；owner run 单飞 + 每 sub-goal 至多一 run；consult/verify 只读可与 owner 并行）
13. **bus.Publish 提交后发布**（run 层执行：run:enqueued 等事件一律事务 commit 后发布）
14. **验证失败 = run failed**（统一走 attempt 重试链；机器失败重试与人 reject 迭代分开计数）
15. **Event 不是真相**（决策 6-4：Event 只是 wakeup hint；判定一律事务内从 DB 重算——Reconcile 幂等，关键状态转移全部 conditional transition）
16. **统一 spawn 入口**（决策 6-4 P0.5：owner run 的创建只经 Coordinator → deriveOwnerAttention → conditional enqueue；verifier 判定/handler 不得直接 spawn）
17. **Workspace 属 run**（决策 6-2：路径即归属；出生 = claim 时刻 branch HEAD；同 agent 不同 workspace 是常态，禁止退回 per-agent 串行）
18. **Change 跨 workspace 交付**（决策 6-3：禁直接 copy 文件；Ready ⇔ 已持久化 Revision；Revision 绑定 Integration Base）
19. **任何 terminal Run 最终必被 Reconcile**（决策 6-11：reconciled_at 与 reconcile 同事务盖章；'' = 崩溃窗口，启动重放；Reconcile 幂等）

---

## 10. 模块边界

| 级别 | 模块 |
|---|---|
| **不动** | store 骨架、goal/run 仲裁核心、daemon 调度、runtime/proto 分层、server/ws、events 骨架 |
| **小改** | `goal.go`（+review 态 + 验收评估 + resolveGate，M0）、`daemon.go`（+验证执行 + verify_timeout + deliver（幂等）+ worktree 分配 + 共享仓 fetch + per-domain 锁（fetch/deliver 共用）+ maxRunDuration + 处理器 agent 派发，M0）、schema（+domain/gate_decision/run.evidence/字段）、web（+审批队列 + 审批页 + 域设置确认卡，M0）、`agentwork-cli`（+request-approval，M2） |
| **新增** | `internal/gate`（卡点规则 + 策略，M2）、`internal/deliver`（git 合入动作，M0 最小版）、`internal/notify`（IM 通知，M1 最小版 / M3 完整版） |
| **后置** | 远程 CI 集成、repo 适配器完整化、多 agent 并发同域、IM 渠道扩展（钉钉/Telegram）、资产域抽象、健康度学习引擎、任务级策略覆盖编译、验证产物漂移治理、失败回收 UI 完整化、备份 |
| **已修复** | bus "同步"文档 vs 异步实现；run:enqueued 事务内发布（不变量 13）；eventForwarder 丢事件 + close 竞态（独立 done channel，ctx 取消不偷取结果） |

---

## 11. 里程碑

| 里程碑 | 内容 | 验收标准 | 状态 |
|---|---|---|---|
| **M0 第一口狗粮** | domain 实体 + worktree（延迟分配）+ **处理器 agent**（NL→checks 编译，可降级手动输入）+ **最小验证**（域 verify 命令 run 后自动跑，verify_timeout）+ **最小卡点流**（review 态 + approve/reject + 自动 deliver（幂等）+ 合并后复验 + 证据包最小版）+ 审批队列最小版 + 演进指标基线记录 + 真实自举任务（verify=`go test ./...` + 结构化约束"diff 必须含 *_test.go" + 覆盖率提升 ≥ N%） | goal 全链路真实走完：执行→验证→审批→自动合入；改动真实可合；指标基线已记；人只点按钮零行代码（决策 2-1） | ✅ 已完成 |
| **M1 无人值守 + 触达** | cron 触发 + maxRunDuration + worktree 生命周期 + 共享仓 fetch + bus 三 bug 修复 + **IM 最小通知（决策 2-14：goal 完成/卡点待审/失败 → 飞书推送，notify adapter 首个实现）** | 睡一觉，飞书里收到真实改进汇报与待审卡；测试全绿（验收不再依赖主动开 Web） | ✅ 已完成 |
| **M2 卡点系统化** | 卡点规则引擎（多规则/表达式）+ request-approval（行为卡点）+ gate_decision 健康度数据 + 验证强度联动生效 + 证据包完整化 + 演进指标趋势展示 | 用户自定义卡点规则生效（"动 config/* 必审"等）；指标趋势可看 | ✅ 已完成 |
| **M3 完整闭环** | IM 完整化：审批卡交互（IM 内 approve/reject）+ 汇报卡片 + 每日摘要 + IM 入站（@agentwork 建任务） | 全程不进 IDE/终端，人只在 IM 出现 | ✅ 已完成（扫码连接 / 卡片审批走 ResolveReview / 日报 / intake 入站） |

---

## 12. 决策记录

| 决策 | 内容 |
|---|---|
| D1 | goal 状态机加独立 `review` 态（与 blocked 对称：等人 vs 等子任务） |
| D2 | 验收判定（机器验证结果 + 卡点规则）在 `ReconcileOnRunEnd` 仲裁事务内，原子推进 |
| D3 | 验收策略是一等概念，**属域**；NL 意图 → 编译检查 → 卡点分流；编译在定义时刻、用户确认后冻结 |
| A1 | 执行工具 = openagent-cli serve --acp（协议连通已确认）；平台不依赖其任何特性 |
| A2 | 在哪个域就在哪干活；`agent.workdir_base` 删除；agent 执行的 goal 必须有域 |
| A3 | 与 A2 统一为 worktree 模型（域共享仓，goal 持 worktree+分支） |
| A4 | 交付自动：approve → 平台 merge + 复验 + push；失败回 review 标注 |
| A5 | 上下文恢复 = 同 worktree（文件态）+ transcript 重放（会话态）；session/resume 留作长驻路径。**已被决策 6-2 修订**：恢复改为分支态 checkpoint（新 workspace 从分支 HEAD 切出）+ transcript 重放 |
| A6 | 验证失败统一 run failed（attempt 重试），验证输出进 result_summary |
| 盲点1 | per-goal run 串行化（service+daemon 双层，不做 DB 约束） |
| 盲点2 | 机器失败重试与人 reject 迭代分开计数（reject 不误触 maxAttempts） |
| 盲点3 | 证据聚合包（diff 统计 + 验证输出 + agent 汇报）入审批卡，M0/M1 就要 |
| 盲点4 | deliver 失败 → 回 review 标注原因；人可重试 deliver 或 reject 回去修 |
| 盲点5 | 协议冒烟已通（用户确认） |
| 盲点6 | maxRunDuration 规则（默认 2h 域可调），超时强制终局 |
| 盲点7 | 验证强度 → 卡点密度联动（弱验证域强制人工段） |
| 盲点8 | 结构化约束是 checks 的第二形态（diff 规则/禁止事项），M0 任务即用 |
| 盲点9 | 迁移机制：M0/MVP 不做（数据可弃）；M1 引入 schema_version 迁移 |
| 原则修正 | 平台能力不得依赖特定 agent 特性（--approver 等一律不用） |
| 决策2-1 | **M0 带最小卡点流**（review 态 + approve/reject + 自动 deliver 串联）；卡点系统完整化在 M2 |
| 决策2-2 | 自主度光谱改写为**卡点密度旋钮**（连续可调），删除"对话驱动"独立模式（同一机制的高频卡点配置） |
| 决策2-3 | **review 期间新 run 不生效**：mention/handoff 只落 comment 不触发 run（证据包不被污染）；后置增强"审批时提示未执行 mention" |
| 决策2-4 | 编译器 = **通用处理 agent（processor agent）**，与其他 agent 同级配置（修正：原"平台内嵌 LLM 调用"升级为此模型——平台自身不调任何 LLM API，所有平台侧 LLM 工作都走处理器 agent） |
| 决策2-5 | 验证强度推断 = **LLM**（与编译由同一处理器 agent 在同一 run 内完成），只做默认值，用户确认后与产物一起冻结、可手改 |
| 决策2-6 | maxRunDuration 超时 → run **cancelled（不耗 attempt）**，goal 保持 active，汇报人，人决定重开/放弃 |
| 决策2-7 | 交付后回滚：**方向记录，实现随远程 CI 集成后置**（"CI 红 → 自动 revert"）；远程 CI 前靠人发现走 failed 回收 |
| 决策2-8 | 编译器降级路径：未配置处理器 agent / LLM 不可用 → 手动输入验证命令（跳过编译），建域不阻塞 |
| 决策2-9 | **deliver 幂等化**：崩溃恢复重新执行，merge/push 前检查跳过已做步骤 |
| 决策2-10 | **per-domain 锁同时护住 fetch 与 deliver**（并发 fetch 的 index.lock 冲突） |
| 决策2-11 | 演进形态：M1 用现有 schedule 起步；**候选队列 + 优先级 + 感知层**（CI 红/依赖过期自动产生候选）为愿景后置 |
| 决策2-12 | 自举愿景补全最后一环：合入 → 重新构建 → 重启 daemon → 新能力生效（自己部署自己；M0 由人手动重启） |
| 决策2-13 | 安全防线分层：**卡点 + guards（平台侧）**，不依赖 agent sandbox；M0 共享 git 凭据，agent 推分支与 deliver 推 main 分权后置 |
| 决策2-14 | **M1 加 IM 最小通知**（goal 完成/卡点待审/失败推送）——解决"无人值守后审批触达"裂缝；M3 完整交互（审批卡/汇报/每日摘要/入站）。**通道：飞书**，notify 按 adapter 模式实现，渠道可插拔 |
| 决策2-15 | **演进指标基线**：M0 建域记 metrics_baseline（测试数/覆盖率），M2 起展示趋势——自举证明的可量化第二层 |
| 决策2-16 | **任务验收强度**：M0 自举任务加"覆盖率提升 ≥ N%"约束（防空测试过关），演示结构化约束完整形态 |
| 决策2-17 | **处理器 run 形态**：run 表加 `run_kind`（worker/processor），与 worker run 同一调度；处理器 agent 建议独立配置 |
| 决策2-18 | **worktree 延迟分配**：第一个 run 开始前才创建，backlog 不占资源 |
| 决策3-1 | **验证环境准备 = setup 段（属验收策略）**：干净 worktree 无依赖（npm build 失败）暴露了环境准备缺口。环境准备是验收策略的一等字段（processor 编译时输出，幂等命令；确认卡可见可改；平台顺序执行 setup→verify，setup 失败=run failed 归因环境）。平台不做技术栈自动检测（违反三角分离 + heuristic 脆弱） |
| 决策3-2 | **确认卡可编辑**：编译产物在冻结前可改（setup/verify/excludes 命令列表 + 完整 JSON）——兑现 §5.3"产物可见、可改、可审" |
| 决策3-3 | **提交排除属域（checks.excludes）**：平台提交 agent 改动时排除的路径由域声明（processor 从仓库 .gitignore/依赖目录编译 + 人确认），**平台零写死**——"仓库该忽略什么"是仓库的领地，平台硬编码 node_modules 等目录既追不上新依赖形式、又可能误排除用户故意跟踪的目录 |
| 决策3-4 | **issue 适配器多 provider（github/gitcode）**：Git 托管平台各说各话（API 形态、webhook 签名头、close 语义不同）——`internal/issue` 定义 Provider 接口（list/comment/close），域级 `issue_provider` 选实现；source_ref 带 provider 前缀；webhook 端点按 provider 分（X-Hub-Signature-256 / X-GitCode-Signature-256 / X-GitCode-Token）；轮询兜底不依赖任何平台特性 |
| 决策3-5 | **远程操作身份 bot 分离**：git_credentials 是平台的远程操作身份（issue 评论/close + git push 都以此账号出现——托管平台 API 硬约束：评论作者 = 认证 token 属主，无法伪装）。配置专用 agentwork-bot 账号的 token（GitHub/GitCode 一致），人的身份保持干净；commit 身份已由 git_identity 独立（"agentwork[bot]"） |
| 决策3-6 | **子 goal 机制休眠**：代码与状态机（blocked/wake/wait-children）保留，但从 agent 引导（AGENTWORK.md、squad briefing、CLI usage）中移除子 goal 与 wait——协作统一走 mention（同 goal 协作 run），agent 创建的 goal 不再默认成为子 goal。理由：子 goal 服务"单 goal 内并行拆活"，两个前提（可靠拆分、合并冲突自修复）当前均未满足——leader 实测 wait 死锁（无子 goal 时永久 blocked，仅可 Cancel）；实际协作（写完→审查→审批）是顺序流，mention 全覆盖；愿景中的并行是平台层面多 goal（issue 轮询天然多 goal），已成立。恢复条件：出现真实并行拆活需求（如大 issue 拆活）+ 冲突自修复落地后，重新启用并补全子 goal 独立交付语义——**已撤销（决策 5-1 复活）** |
| 决策4-1 | **评论即重开**：终态 goal + **human 作者** + 评论含 **action mention**（agent/squad）→ 自动 Reopen 再触发（"这个任务还没完"）；纯评论仅落地。agent/system 评论不触发 |
| 决策4-2 | **mention pingpong 双阈值**：agent 触发 run 数 &gt;4 注入协作警告 / ≥8 强制 failed（协作循环判死）；human/system 触发不计入 |
| 决策4-3 | **guest run 失败留痕**：协作 run 失败（带 trigger_comment_id）写系统评论，不重试、不动 goal 状态——失败在 feed 可见 |
| 决策4-4 | **squad 审查 checkpoint 平台化**：goal 进 review 时平台自动 mention role=reviewer 成员（排除 leader 自审）并 enqueue 审查 run；审查意见进审批卡供人决策——审查是机制，不是 agent 自觉。**每个 completed run 的汇报由平台落 feed**（完整保留、不截断、不查重；guest 与 assignee 一律落——审批上下文必须完整）。**2026-08 修订（Option B，reviewer 先行意见、人零等待）**：审批卡在停靠瞬间照发，但带"🔎 审查中：X、Y——可等待他们的意见后再决定"提示（点名 pending review run 的 reviewer）；`goal:review_ready`（本轮审查 run 全部终态 / 无 reviewer / 10min 兜底）触发平台 **PATCH 同一张卡**，把提示替换为真实审查意见。卡片的"审查意见"段只取 role='review' run 的评论（worker 汇报是证据不是意见）。reviewer 从不阻塞 goal 状态机（意见无判定权，人唯一权威）；卡片 message_id 记在内存，重启落在 park 与 ready 之间则补发整卡 |
| 决策4-5 | **策略缺陷客观检测**：verify 命令 exit 127（POSIX command not found）→ 自动标注"疑似验收策略问题"系统评论——owner 修策略而非 agent 白烧重试（替代字符串匹配） |
| 决策4-6 | **评论区语义**：评论区 = 协作交流区，只承载人的话与协作动作（创建/改派/审批理由/重开/对话/mention）；系统内部状态不入评论（状态史归活动日志 + 执行流）。评论 = 团队上下文的对话层——每个 run 注入完整评论 feed（不限作者、不限条数） |
| 决策4-7 | **评论注入压缩（规划未实现）**：全量注入随评论增长由平台负责压缩——预算制分层：决策记录/触发评论永不进摘要（保真层）、最近 ~4K 估算 token 原文（保留层）、更早的进平台累积摘要（背景层）。摘要 = 平台调度的 processor run（run_type=summary），触发 = 未摘要窗口达 ~8K 估算 token，每次压最老的到回落保留预算；估算 token = 启发式（CJK×1.2 + 非CJK×0.28，±30% 只影响触发时机）。失败：事件驱动重试（新评论才再试，连续 3 次降级常驻），摘要失败不影响 goal 状态，降级 = 机械压缩（保留层原文 + 更早「作者: 首句」） |
| 决策4-8 | **client 执行环境代理（已实现）**：双通道——ACP fs/terminal RPC（8 handler，含 request_permission 自动放行=工具许可非审批）+ MCP 工作区 server（官方 go-sdk，streamable HTTP，/mcp/{runID} 挂 GET+POST，session/new McpServers 注入；给工具不走 client RPC 的 agent 如 opencode，实测 agentwork_* 工具落回 worktree）。统一模型：stdio/ws/tcp 同机制（注册 handler、声明能力、注入 Workspace 指引）。terminal per-command，session 关闭统一清理。无路径限制（信任边界 = daemon 用户）；fs 敏感文件拦截、MCP capability token 列后置 |
| 决策4-9 | **平台终止过期 run（已实现，2026-08 修正）**：goal 易主（handoff）时平台终止旧 owner 的 running run——runID→cancel 注册表 + goal:assigned 事件驱动；claim→注册窗口 DB 兜底标记 + 注册后自查归零。取消原因结构化（run.cancel_reason + reason_code，不用字符串匹配）。协作语义：接力不是等待（同 goal 串行，依赖对方=完成部分→mention→结束→被拉回，A5 恢复模型）。AGENTWORK.md 引导。修正：原"进 review 时终止 run"（approval cut）已移除——agent 不再请求审批（决策 4-11），gate 命中时触发 run 已自然完成，无需杀 |
| 决策4-11 | **agent 无权请求审批（已实现）**：移除 `goal request-approval`（CLI/API/引导）——审批触发 = 机器 gate + 人；执行者不得请求判定（三角分离）。"什么时候要人审"由卡点规则表达；squad 审查 run 是 review 窗口的平台机制（意见供参考，非审批对象） |
| 决策4-12 | **人工停止 run（已实现）**：`POST /goals/{id}/runs/{runId}/stop` 终止 running run（执行层操作，goal 状态不动，恢复由人决定）；Cancel goal 同时终止 running run；stdio Close 进程组杀 + 关读端（停止即刻生效） |
| 决策4-13 | **MCP 协作工具替代 CLI（已实现）**：协作动作（评论/交接/查看）直接做成 MCP 工具（agentwork_goal_comment / goal_assign / goal_list / agent_list / squad_list）——结构化参数、schema 自带、零学习成本；agent 不再学 CLI 语法（曾实测 agent 为搞清 CLI 用法去读平台源码）。agentwork-cli 保留二进制（human 调试），agent 引导全部指向 MCP 工具 |
| 决策4-10 | **pingpong 阈值保持 4/8**：接力链每跳都计入 agent 触发（防甩锅口径），4 注入收敛警告 / 8 判死（可 Reopen 恢复、计数清零）；误伤"需求拆解差的长链"的代价 < 放过真甩锅链 |
| 决策5-1 | **子 goal 复活（撤销决策 3-6）**：Collaboration.md 全盘采纳——协作四行为模型（Comment 说 / Consult 问 / Handoff 接力 / Sub-goal 拆活）要求子 goal 承担"拆活"路径；`CreateSubGoal` 恢复引导（AGENTWORK.md + MCP `create_sub_goal`），机制复用既有 WaitChildren/wakeParentIfReadyInTx/wake_count，父完成不自动完成。与 3-6 的区别：拆活入口从"agent 自建 goal"收窄为显式工具 + owner 权限（决策 5-6） |
| 决策5-2 | **MCP 协作工具面重构（替代决策 4-13）**：移除 goal_comment/goal_assign；新工具 comment_goal（说）/ consult_agent（问）/ handoff_goal（接力）/ create_sub_goal（拆活）/ goal_wait（挂起等子完成），保留 goal_list/agent_list/squad_list。语义显式化：mention 不再承担任务委派或所有权转移。**2026-08 经 6-10 修订**：goal_wait 随 blocked/wait 退役移除，工具面扩充 cancel_sub_goal/verify_sub_goal/integrate_change/get_change/get_sub_goal/get_verification |
| 决策5-3 | **guest run 只读平台化**：consult/review run 不 commitRunChanges；平台在 run 结束时丢弃 guest 窗口期新产生的未提交改动（entry 脏路径快照求差，entry 已脏路径保留——A5 恢复模型）；guest 直接 commit 检测到写系统评论提醒（不 revert，无法无损还原 entry 状态） |
| 决策5-4 | **run.role 显式化**：run 表加 role（owner\|subgoal\|consult\|review\|verify，2026-08 经 6-9 扩展），enqueue 时派生（isLeader/无 trigger→owner；trigger 评论 system→review、其余→consult）；角色是信息快照，归属判定仍按 reconcile 时刻动态查表（不变量不变） |
| 决策5-5 | **run 状态 handed_off**：handoff 终止的旧 owner run 记为 handed_off（cancel_reason=handoff 保留），不再是 cancelled——"交接"不是取消/失败；不耗 attempt、不自动重试、notify 不推中断卡（同 cancelled+handoff 规则）；脏检查逃生门扩展（cancelled/handed_off/guest/review 历史均视为可归属）——**已撤销（决策 6-6：handed_off 回滚为 cancel_reason 语义，status 保持五态）** |
| 决策5-6 | **handoff 权限（Invariant 2）**：只有当前 owner 能 handoff——service 层 Assign 增加 actorType/actorID 参数（HTTP 不传=human）；agent actor 校验：agent 目标=assignee、squad 目标=leader、human 目标拒绝。HTTP assign 保持现状（单机单用户，HTTP 面=human 操作） |
| 决策5-7 | **handoff 循环检测（警告+审停）**：按 handoff_event 计数，≥4 系统评论警告、≥8 goal 进 review 停靠（人 approve 继续/reject 驳回）——协作循环是协作异常不是任务失败，不判 failed（与 mention pingpong 4/8 判死不同） |
| 决策5-8 | **consult 闭环自动恢复**：consult_request 表记录完整链路（requester/trigger/guest/response）；guest run completed 且 requester 是 agent 且仍是 owner 且 goal active → 平台自动 enqueue requester 的下一 run（attempt 1，trigger 留空——不叠加 4-2 计数）；guest 报告评论回填 response_comment_id |
| 决策5-9 | **handoff_event 表**：所有权转移 append-only 审计（from/to 多态 + from_run_id/to_run_id + reason + actor）；Timeline 第四段集成 |
| 决策5-10 | **goal:finished 只在 goal 真正终态发布**（修 bug）：ReconcileOnRunEnd 尾部无条件发布改为——done/failed 才发（携带 goal 新状态）；retry 不发失败卡、review 停靠不发完成卡、run cancel/handed_off 不发 goal 事件 |
| 决策6-1 | **子 goal 形态重定义（修订 5-1）**：SubGoal 是独立实体（新表），不是 child goal——依附 goal 生命周期、不可递归（schema 无 parent 列 + 工具只收 goal_id + owner 权限三重保险）、assignee/verifier 分离、无独立交付。完整模型见 Collaboration.v2.md（Approved） |
| 决策6-2 | **run 级 Workspace/Worktree（修订 A5）**：Workspace 按 Run 实例化（`runs/<runID>/` 路径即归属）；workspace 出生 = claim 时刻 branch HEAD；恢复模型从"文件态复用"改为"分支态 checkpoint"（run 结束 commitRunChanges → 新 workspace 从 branch HEAD 切出 + transcript 重放）；**worktree 是暂态**——git 一个分支只能一个 checkout，run 结束立即释放（成功失败皆然；崩溃遗留由启动 sweep 清理 + worktree prune，未提交 WIP 丢失是接受的代价）；脏检查逃生门/归因机制退役。术语改名：`workspaces/`→`runs/`、`workspaceRoot()`→`runsRoot()`、`goalWorktreePath()`→`runWorktreePath()`、MCP `Executor.Workdir`→`Worktree` |
| 决策6-3 | **Change + Revision + Integration**：Change 是逻辑交付物（4 态 ready/integrating/integrated/conflict），Revision 绑定 Integration Base（base_ref/head_ref）；Change 与第一版 Revision 同事务原子创建（Ready ⇔ 已持久化 Revision）；冲突修复 = assignee 新 workspace 从当前 Goal HEAD 切出重应用 → 同 Change 追加 Revision N+1 回 ready；Integration 由 owner run 经 `integrate_change` 工具执行（平台在 run worktree 执行 merge，Coordinator 不做 git）——**已实现** |
| 决策6-4 | **Goal Coordinator（Event 驱动 Reconcile，不是业务决策）**：Event 只是 wakeup hint，DB 是唯一真相；`ReconcileGoal` 事务内重算 + `deriveOwnerAttention()`（integration/recovery/user_action）+ conditional enqueue（幂等）；Latch 双触发（sub_goal/change changed 与 run.terminal 都触发 Reconcile）；P0.5 统一 spawn 入口（`EnqueueOwnerRun`/`EnqueueOwnerRunTx`，create/assign/reopen/reject/reconcile 全部经此）；新增 `run.terminal` 事件；恢复统一为 successor run；**Owner Resume Context 索引式注入**（attention 项 + change/sub-goal 清单，详情经 get_change/get_sub_goal/get_verification 展开）；cancelled owner run 后不自动 spawn（防超时循环）；**无进展不重唤**：spawn 仅在 attention 信号（change revision / failed sub-goal）新于该 goal 最近一次 owner spawn 时发生（防"唤醒→不干活→再唤醒"忙循环，E2E 实测 7 连唤）；per-goal reconcile 互斥锁消除事件风暴下的 SQLITE_BUSY——**已实现**。**两种唤起机制并存（2026-08 注记）**：① Signal-driven——attention 信号（change ready / failed sub-goal / verified wrapup）→ Reconcile → spawn；② Direct workflow transition——assign/activate/reopen/reject/create-active 的**事务内** enqueue（决策 6-15②）。二者共享四件套：Run creation（enqueueTx+coalesce）、Claim policy（决策 6-15①）、Causal trigger（run.trigger_comment_id）、Single-flight（Claim 守卫）——不允许出现第三套语义 |
| 决策6-5 | **Verifier 机制**：Sub-goal 三角色 assignee/verifier/owner；默认机器当 Verifier（域验证命令），可选 agent verifier（machine\|agent，无 human）；两层失败分离——`execution_attempt`（机器失败 ≤3 → Sub-goal Failed 报 owner）vs `quality_iteration`（verifier reject 不设上限 → 回 Running 新 run）；verifier 只产生判定（`verify_sub_goal` 工具 → verification_result + 事件），spawn 走 post-commit 显式 enqueue；verify run 只读（detached 快照）；Review（决策 4-4，无判定权）与 Verify（有判定权）正式区分；owner 可 `cancel_sub_goal`（级联停 run）——**已实现** |
| 决策6-6 | **Handoff 语义扩展 + handed_off 状态撤销（撤销 5-5）**：Handoff 是 Goal Owner 的变化不是 run 生命周期新状态——run.status 回五态，`cancel_reason=handed_off` 承担语义；handoff 只终止 owner 角色 run（sub-goal/consult/verify runs 继续，onGoalAssigned 按角色过滤）；forced handoff（MVP 唯一形态）：kill 后**不代提交 WIP**（mid-git-op 不安全），WIP 留 `runs/<old-runID>/` 取证，新 owner 从 branch HEAD 切新 workspace；deliver 只等 owner run（`deliverWaitForOwnerRuns`）；handoff_event 表保留；循环 4/8 审停不变 |
| 决策6-7 | **consult 恢复路由 + 只读口径**：恢复目标 = requester_run_id 的 successor run（按 requester run 角色路由，禁止 goal_id→owner）；consult 只读 = **domain read-only**（不改 goal/sub-goal/change 状态；workspace ephemeral 写允许；不产生 Deliverable/Change） |
| 决策6-8 | **Goal 级联与 attention**：Cancel 级联停 sub-goals/runs（Change/Verification 历史保留）；Delete 物理级联（软删除后置）；Reopen 不复活旧 sub-goal；`goal.attention` 列持久化派生状态，human owner attention → `goal.attention_needed` 事件 → IM 卡（不 spawn）；当前版本所有 sub-goal 均 required；**fan-out ≤20 active**（历史不限）；无代码 sub-goal = verified 且无 Change（交付物走评论 feed）；**owner run 终局守卫**：存在非终态 sub-goal 或 ready/conflict change 时，owner run 完成不进入卡点判定（goal 保持 active，attention 循环接管下一步）——人门只在最终状态触发——**已实现** |
| 决策6-9 | **run.attempt 降级为实例序号**：重试判定权威移到实体计数（goal.execution_attempt / sub_goal.execution_attempt + quality_iteration）；run 是一次执行实例（sub-goal 与 run 是 1:N，关系真相 = run.sub_goal_id，无 current_run_id 指针）。**实现注记**：sub_goal.execution_attempt/quality_iteration 已为权威；goal 层重试仍由 run.attempt 承担，goal.execution_attempt 列建而未用——对齐待做 |
| 决策6-10 | **v2 数据模型与工具面**：新表 sub_goal/change/change_revision/verification_result；MCP 工具面 = comment_goal/consult_agent/handoff_goal/create_sub_goal/cancel_sub_goal/verify_sub_goal/integrate_change/get_change/get_sub_goal/get_verification/goal_list/agent_list/squad_list；**blocked/wait/parent 机制已退役**（WaitChildren/wake/wake_count/goal.parent_id 全部删除，goal 状态机回五态）——**已实现** |
| 决策6-11 | **Run terminal 与 Reconcile 的原子边界（P0-1）**：`run.reconciled_at` 在 reconcile 事务内盖章——terminal 状态与 reconcile 同生共死；'' = terminal-but-unreconciled（崩溃窗口），daemon 启动 `ReconcilePendingTerminal` 重放（幂等：条件转移 + 汇报评论与盖章同事务，重放不重复；从未开跑的 cancelled run 无 reconcile 语义，跳过）。不变量 I2：**任何 terminal Run 最终必被 Reconcile，且 Reconcile 幂等**——**已实现** |
| 决策6-12 | **边界钉死批次（P0-2/P1-1/P1-2/P1-3）**：① CreateSubGoal 与初始 run 同事务（幽灵 sub-goal 不可能）；② 仅 active goal 可拆活（review = execution freeze 点，terminal/backlog 拒绝——状态机不自环）；③ verifier verdict 强校验（role=verify + sub_goal_id + agent_id==verifier_id + sg.verifying + run.running）；④ conflict 不武装 owner attention（rework 是 assignee 的责任，新 revision 回 ready 才唤醒 owner；终局守卫仍把 conflict 算 pending，goal 不会带冲突进卡点）——**已实现** |
| 决策6-13 | **Successor Run 与状态转移同事务 + 启动全量 reconcile（P0-3）**：所有 afterCommit enqueue（sub-goal 重试/verifier/reject rework/conflict rework、owner 重试、consult 恢复）搬进各自 reconcile 事务——状态转移与其后继 run 同生共死，崩溃窗口不再丢后继 run（事件只负责 publish，DB 写全在事务内）；daemon 启动 `ReconcileAllActive` 遍历 active goal 重跑幂等 ReconcileGoal——latch 事件在提交后丢失也能从 DB 真相重推导 attention 并补 spawn。Event≠Truth 的完整恢复面——**已实现** |
| 决策6-14 | **backlog 激活路径（`POST /goals/{id}/activate`）**：backlog → active 的第三入口（此前只有创建与 Reopen）——未指派而创建的 goal 有了回到流水线的路。条件转移（仅 backlog），spawn 走 P0.5 统一入口；顺带修正 `EnqueueOwnerRun` 的 human 契约（文档声称 no-op、实现报错——Reopen 人工 goal 的潜在雷同处修复）——**已实现** |
| 决策6-15 | **执行安全闸门批次（协作状态机收敛）**：① **Claim 是唯一执行闸门**——`goal.status='active'` 才可 claim，`review` 仅 `role='review'`（平台审查 run）可 claim，其余不可；processor run（无 goal）豁免。review 冻结 execution 不冻结 intent（决策 2-3 修订）——入口只负责产生 queued intent，Claim 负责放行；② **Successor 原子化**——Create-active/Assign(handoff)/Activate/Reopen/Reject 的状态转移与后继 run 同事务（Assign 在事务内按最终 status 决策 enqueue 并回填 handoff_event.to_run_id；HTTP assign 与 MCP handoff_goal 的调用方 enqueue 删除，统一走 Assign）；③ **Reconcile per-goal mutex 永不删除**（删除与等待者竞态会让同 goal 两个 Reconcile 并行）；④ **ownRunByGoal 加 role 门**——agent 目标要求 `role='owner'`：mention 触发的 consult run 落在 assignee 身上不再拥有 goal 权威（Invariant 6），走 guest 语义；⑤ **终局守卫口径**——pendingConsults 只统计 requester 是当前 owner 的 consult（human mention 的 guest 不拦卡点；修"human consult 在飞 + owner turn 结束 → 无人恢复 → 死 active"）；⑥ **late-result 防护**——`Finish`/`failProcessorRun` 条件更新 `WHERE status='running'`：runaway reaper/交接窗口戳已终态的 run，迟到的 agent 结果不得覆盖终态、不得推进 goal；run:cancelled 事件由"实际完成覆盖的一方"单发；⑦ **runaway reaper**——daemon 周期扫描超过 per-domain max_run_duration+300s 宽限仍 running 的 run，条件盖 cancelled + reason=`runaway`（kill 走 cancel 注册表，不阻塞），notify 推"任务中断"卡交人；⑧ **事件路由修正**——`run.terminal`（点）与前端/hub 的 `run:terminal`（冒号）统一为点；hub 白名单补 sub_goal.verifying/retrying、squad:member_removed；squad 审查 dedupe 只数 role='review'（D-1 下 review 期排队的 human mention 不得顶掉审查请求）；⑨ **consult status 注入**——owner prompt 增加 "## Consult status"：以 consult_request（requester_run_id/response_comment_id 因果）为主查询、时间仅作 scope 过滤，注入"问→答/失败"；⑩ **sub-goal 派发链**——create_sub_goal 的派发评论与 sub_goal、初始 run 同事务，run.trigger_comment_id 指向派发评论（汇报自动引用）；pingpong 计数豁免 role IN ('subgoal','verify')（当前近似口径，最终语义 = interaction edge，非 role）；⑪ **get_comments MCP 工具**——只读、after cursor + limit，长 run 主动拉新评论——**已实现** |

---

## 13. 后置问题清册

- 远程 CI 集成（M0/M1 的验证 = 本地命令）
- 多 agent 并发操作同一域（分支冲突、merge 冲突的 agent 自修复）
- 汇报卡片的证据聚合智能化（LLM 摘要）
- 任务级策略覆盖的 NL 编译流程
- 验证产物漂移治理（仓新增技术栈 → 提示重新编译）
- 失败后的人工接手路径（failed goal 重开/改派 UI）——已实现（Reopen / 改派 / 评论即重开）
- **human 主人关单路径（2026-08 评估结论）**：human-assigned active goal 无独立关单端点——人改派给 agent 走正常闭环即可；暂不新增 `POST /goals/{id}/close`（"理论上应该有"不构成理由）。将来若 human 承担更多执行，再评估
- **worker consult（2026-08 评估结论）**：暂维持 owner-only（v2 矩阵允许 sub-goal assignee consult 是已知偏差）——放开的前提是 v2 §9 的恢复角色路由（requester 是 sub-goal run → 恢复该 sub-goal 的 successor，而非 goal 级 owner run）；届时一并实现，勿单独放开
- 多域设置向导（NL → 编译 → 确认的交互形态）
- 备份/数据安全策略
- 成本追踪
- worktree 生命周期清理的数值（保留天数）
- 健康度学习引擎（连批 20 次建议删门；连拒 3 次建议收紧）
- squad 子 goal 独立合入 main，父 goal 只做协调汇总（方向已定，实现后置）——子 goal 机制已复活（决策 5-1 撤销 3-6），独立交付语义待一并实现
- 审批时提示"review 期间有 N 条未执行 mention，是否执行"（决策 2-3 的代价缓解）
- 远程 CI 集成时配套"CI 红 → 自动 revert"回滚规则（决策 2-7）
- 域类型只实现 repo；文档/配置/知识库/backlog 等资产域后置
- IM 入站指令（@agentwork 建任务）——已实现（M3 完成：intake 解析 / 多域草稿澄清 / 飞书已接入）
- Git 触发（webhook）——已实现（`/webhooks/github` + `/webhooks/gitcode`，HMAC 验签；webhook secret 平台级共享，空 = 禁用 webhook 仅靠轮询）
- notify 渠道扩展（钉钉/Telegram/微信等）——飞书为首个实现（决策 2-14），adapter 模式可插拔
- 候选队列 + 优先级 + 感知层（CI 红/依赖过期自动产生演进候选）——"全自动演进"的日常形态（决策 2-11）
- 自举部署闭环：合入 → 重新构建 → 重启 daemon（决策 2-12）
- 处理器 agent 的扩展用途：transcript 摘要、汇报生成、卡点健康度分析（决策 2-4 推论）
- git 凭据分权：agent 推分支与 deliver 推 main 分离（决策 2-13）
- squad leader 变更不触发旧 leader running run 的终止（squad 无 leader 变更事件；目前后果是旧 leader run 跑完丢弃 + 协作中断，需人手动——接决策 4-9 机制时补 squad:leader_changed 事件）
