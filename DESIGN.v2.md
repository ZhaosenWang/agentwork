# agentwork v2 — 产品与架构设计（冻结版）

> 本文档是 agentwork v2 的产品定义与架构基线，2026-08-07 与 eushing 逐条对齐后冻结。
> 覆盖：产品定义 / 闭环逻辑 / 核心概念 / 状态机 / 验收策略 / 工作区模型 / 交付 / 执行 / 不变量 / 模块边界 / 里程碑 / 决策记录。
> 现有 DESIGN.zh.md 记录 v1 实现现状；本文档是产品级修订的方向基线，实现以本文为准。
> **架构冻结的含义：本文档之后的代码都在这份设计上长；任何改动本文档的提议都先改文档、后改代码。**

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

注：① 触发目前仅 cron/Web（IM 入站/Git 触发为未来项，见 §13）；⑦ 汇报：M1 起 IM 最小通知（goal 完成/卡点待审/失败推送），M3 完整交互。

**人只出现在 ⑤ 卡点**，其余全自动。

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

---

## 3. 数据模型变更（相对 v1）

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
    worktree_path / branch_name（goal 的 worktree 与分支）
    review_request（卡点触发原因/证据包指针）
    human_iterations INT（人 reject 迭代计数，与 run.attempt 分开——盲点 2 定案）
+ run 表新增:
    run_kind(worker/processor——处理器 agent 的内部 run 与 worker run 同一套调度机制，决策 2-17)
    evidence TEXT(JSON)（证据包：diff 统计 + 验证输出 + agent 汇报摘要；卡点触发时生成，审批卡引用）
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
                  ↘ blocked（等子任务，已有）
                  ↘ review（等人类决策，新增，与 blocked 对称）
                  review → done（approve + deliver 成功）
                  review → active（reject → 新 run 带决策 note；或 deliver 失败后人工决定）
run:   queued → running → completed | failed | cancelled（不变）
```

### 关键时序（验收判定在仲裁事务内）

```
agent 干完（run 到终态）
→ daemon 跑域验证策略（事务外：go test 等耗时命令 + 结构化约束检查）
→ 验证结果落 run
→ 触发 ReconcileOnRunEnd（事务内）：
    读验证结果 + 卡点规则
    → 全过且无卡点命中 → done
    → 验证挂 → run failed → attempt 重试 → 耗尽 → goal failed（报人）
    → 卡点命中 → review（等人）
→ 人 approve/reject（comment 摄入）→ 唤醒（resolveGate，镜像 wakeParentIfReadyInTx）
→ approve: 触发 deliver（§6）；reject: 新 run 带决策 note（同 worktree）
```

### 规则

- **per-goal 串行化**：同一 goal 至多一个活跃 run（worktree 不可并发写）。service 层 enqueue 检查 + daemon 调度层保证，不做 DB 约束。
- **attempt 分开计数**：机器失败重试（`run.attempt`，上限 3）与人类 reject 迭代（`goal.human_iterations`，不设硬上限）分离，合法迭代不得被误掐断。reject 的触发源记入 `trigger_comment_id`（已有字段）。
- **maxRunDuration（决策 2-6）**：单 run 最大时长（默认 2h，域策略可调），超时 → run 记 **cancelled（不消耗 attempt）**，goal 保持 active，汇报人"任务超时"——人决定重开或放弃（重开 = 新 run 继续同一 worktree）。与 idle watchdog（静默）互补。
- **verify_timeout**：验证命令自身超时（默认 10min，域策略可调），超时视同验证失败（防挂死的 verify 让 run 永远 running）。
- **review 期间新 run 不生效（决策 2-3）**：goal 在 review 时，mention/handoff 只落 comment、**不触发 run**（审批针对的分支状态冻结，证据包不被污染）。审批通过后由人决定是否执行（后置增强：审批时提示"review 期间有 N 条未执行 mention"）。
- **worktree 干净性（C4 定案）**：run 开始前 git status 检查——存在无法归因的改动（如人为修改）→ 该 run 不启动，goal 进 review 等人工处理；改动可归因于历史 run（超时/取消遗留）→ 视为预期，继续。
- **cancel/delete 语义**：Cancel 覆盖 review 态；delete 时级联清理 worktree（并入 worktree 生命周期）。

---

## 5. 验收策略（一等概念，属域）

### 5.1 结构

```
验收策略 = 完成判定的完整定义
├── 环境准备（setup）：依赖安装等验证前置（npm install 之类，幂等；平台在干净 worktree 上执行，依赖不会自己存在）——M3 定案（决策 3-1）
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

## 6. 工作区模型（A2+A3+A5）

### 6.1 域持共享仓，goal 持 worktree

```
domain（agentwork 仓）
 └── 共享仓（daemon 持有，只 clone 一次）
      ├── worktree-<goal-A> ← 分支 feat/xxx（任务 A 的工作区）
      ├── worktree-<goal-B> ← 分支 feat/yyy（任务 B 的工作区）
      └── ...
```

- "在哪个域就在哪干活"：goal 挂 domain → daemon 分配该域一个 worktree（独立目录 + 独立分支）→ run 在此工作
- 多任务并行：每 goal 一个 worktree 互不干扰（git worktree 是业界并行 agent 标准做法）
- 同一 goal 的 run **顺序复用同一 worktree**（per-goal 串行，§4）
- **延迟分配（决策 2-18）**：worktree 在第一个 run 开始前才创建（goal 先记录目标 branch），backlog 挂起不占磁盘资源
- **共享仓同步（B5 定案）**：每次 run 开始前 daemon fetch 共享仓（分支 base 新鲜）；deliver 时 pull 主分支。**fetch 与 deliver 共用同一把 per-domain 锁（决策 2-10）**——并发 fetch 同一共享仓会 index.lock 冲突
- worktree 生命周期：goal 终态后保留 N 天再清理（后置，daemon 定时任务）

### 6.2 上下文恢复（A5，CPU 中断模型）

| CPU 中断 | agentwork 卡点 |
|---|---|
| 保存现场（寄存器/PC） | run 事件持久化到 `chat_message` + 改动留在 worktree |
| 处理中断 | 人审批（review 态） |
| 恢复现场 | 新 run 回到同一 worktree（文件态）+ 重放上轮 transcript（会话态） |
| 继续执行 | agent 从被打断处继续 |

- **M0/M1：swap 模型**——现场存在 SQLite 和磁盘，等多久都不怕（进程不保活，机器重启不丢）。
- **长驻路径**：ACP `session/load`/`session/resume`，`session_id` 已记录；等 agent 支持服务端会话时升级为进程级恢复。
- transcript 重放：只重放被中断那轮的 transcript；超阈值截断/摘要（摘要后置）。
- 新 run 开始前检查 worktree 干净性（git status），脏则报人。

---

## 7. 交付（A4：自动合入）

```
卡点 approve → daemon 在 goal 的 worktree 执行 deliver（确定性脚本，无 LLM）：
  git checkout <default_branch> → git pull → git merge --no-ff <goal分支>
  → 合并后【再跑一遍域验证】（合并后的 main 必须绿）
  → git push → goal 层 MarkDelivered → done
失败（冲突/验证红）→ 不 push，goal 回 review 标注失败原因
  → 人可：重试 deliver / reject 回去让 agent 修（reject 语义天然覆盖）
```

- 合入是**平台动作不是 run**——不派 agent 去 merge（避免"干活的人自己验收自己"）
- 验证执行两处：run 结束 + 合入前（保证进主分支的代码是绿的）
- **同域 deliver 串行化（B6 定案）**：同一 domain 的多个 deliver 排队执行（daemon per-domain 锁），避免多 goal 并行开发时并发 push main 冲突
- **deliver 幂等化（决策 2-9）**：daemon 崩溃重启后，发现"goal 在 review 且已 approve 但未 done"→ 重新执行 deliver；merge 前检查（分支已合 → skip）、push 前检查（已推 → skip）——半途崩溃不丢步骤
- git 凭据/身份入域配置（如 `agentwork[bot]`）
- **交付后回滚（决策 2-7，方向记录）**：远程 CI 集成（后置）时配套"CI 红 → 自动 revert"规则；远程 CI 之前没有客观失败信号，靠人发现后走 failed 回收路径
- 这一步是 repo 适配器的种子，M0 必做

---

## 8. 执行层（A1）

- **M0 执行工具：`openagent-cli serve --acp`**（`~/opensource/openagent-go/build/openagent-cli`，ACP over stdio）——与 acpbackend 协议连通性已验证。
- runtime 配置：transport=stdio, provider=acp, executable=openagent-cli 路径, args=["serve","--acp"]。
- **openagent-cli 只是执行工具**——不依赖它的任何特性（--approver/--channel/--summarizer 等一律不使用不响应）。见 §9 不变量。
- **处理器 run 形态（决策 2-17）**：处理器 agent 的活也是 run（`run_kind=processor`，挂 domain 不挂 goal），与 worker run 同一套调度机制；处理器 agent 建议独立配置（与 worker 共用同一 CLI 时，per-agent 信号量会让编译 run 排队等 worker run 完成——M0 单 agent 场景无碍）。
- 备选：claude（jsonl，需先实现 jsonlbackend）、opencode——后置。
- idle watchdog（已有）：静默 window 2min / tool 在飞 10min。

---

## 9. 工程不变量（v1 七条保留 + v2 新增）

v1 保留：
1. run 无权威，goal 层唯一权威（任何影响 goal 状态的写入走仲裁）
2. 状态机解耦（执行面/产品面互不拉扯）
3. 不解析 agent stdout 提取意图（agent 主动调 CLI 产生结构化副作用；mention 是结构化 URI）
4. backlog 语义不变量（指派进 backlog 不启 run）
5. 每 goal+agent 至多一 pending run（coalescing）
6. squad 不跑活（只路由到 leader，status authority 单独门控）
7. per-task workdir 隔离 → 升级为 §6 worktree 模型

v2 新增：
8. **验收判定在仲裁事务内**（卡点判定与状态推进原子，不建在事件订阅上）
9. **三角分离**：定义（域所有者）≠ 执行（worker agent）≠ 判定（平台机器+人）。**worker agent 永不定义验证标准**；NL→checks 编译由独立配置的处理器 agent（processor agent）完成，产物必须经用户确认后冻结——用户确认是"定义权归人"的最终保障
10. **平台能力不依赖任何特定 agent 特性**——agent 的唯一职责是执行，agentwork-cli 是唯一跨 agent 协作通道；卡点/验证/交付/通知/摘要全部平台自建
11. **验证与执行者无关**（同一任务谁干都按同一标准验）
12. **per-goal 串行化**（worktree 不可并发写）
13. **bus.Publish 提交后发布**（v1 已立，v2 要求 run 层执行——修复 v1 的 run:enqueued 事务内发布）
14. **验证失败 = run failed**（统一走 attempt 重试链；机器失败重试与人 reject 迭代分开计数）

---

## 10. 模块边界

| 级别 | 模块 |
|---|---|
| **不动** | store 骨架、goal/run 仲裁核心、daemon 调度、runtime/proto 分层、server/ws、events 骨架 |
| **小改** | `goal.go`（+review 态 + 验收评估 + resolveGate，M0）、`daemon.go`（+验证执行 + verify_timeout + deliver（幂等）+ worktree 分配 + 共享仓 fetch + per-domain 锁（fetch/deliver 共用）+ maxRunDuration + 处理器 agent 派发，M0）、schema（+domain/gate_decision/run.evidence/字段）、web（+审批队列 + 审批页 + 域设置确认卡，M0）、`agentwork-cli`（+request-approval，M2） |
| **新增** | `internal/gate`（卡点规则 + 策略，M2）、`internal/deliver`（git 合入动作，M0 最小版）、`internal/notify`（IM 通知，M1 最小版 / M3 完整版） |
| **后置** | 远程 CI 集成、repo 适配器完整化、多 agent 并发同域、通知/IM、资产域抽象、健康度学习引擎、任务级策略覆盖编译、验证产物漂移治理、失败回收 UI、备份 |
| **必须修（M1 前）** | bus "同步"文档 vs 异步实现；run:enqueued 事务内发布；eventForwarder 丢事件 + close 竞态 |

---

## 11. 里程碑

| 里程碑 | 内容 | 验收标准 |
|---|---|---|
| **M0 第一口狗粮** | domain 实体 + worktree（延迟分配）+ **处理器 agent**（NL→checks 编译，可降级手动输入）+ **最小验证**（域 verify 命令 run 后自动跑，verify_timeout）+ **最小卡点流**（review 态 + approve/reject + 自动 deliver（幂等）+ 合并后复验 + 证据包最小版）+ 审批队列最小版 + 演进指标基线记录 + 真实自举任务（verify=`go test ./...` + 结构化约束"diff 必须含 *_test.go" + 覆盖率提升 ≥ N%） | goal 全链路真实走完：执行→验证→审批→自动合入；改动真实可合；指标基线已记；人只点按钮零行代码（决策 2-1） |
| **M1 无人值守 + 触达** | cron 触发 + maxRunDuration + worktree 生命周期 + 共享仓 fetch + bus 三 bug 修复 + **IM 最小通知（决策 2-14：goal 完成/卡点待审/失败 → 飞书推送，notify adapter 首个实现）** | 睡一觉，飞书里收到真实改进汇报与待审卡；测试全绿（验收不再依赖主动开 Web） |
| **M2 卡点系统化** | 卡点规则引擎（多规则/表达式）+ request-approval（行为卡点）+ gate_decision 健康度数据 + 验证强度联动生效 + 证据包完整化 + 演进指标趋势展示 | 用户自定义卡点规则生效（"动 config/* 必审"等）；指标趋势可看 |
| **M3 完整闭环** | IM 完整化：审批卡交互（IM 内 approve/reject）+ 汇报卡片 + 每日摘要 + IM 入站（@agentwork 建任务） | 全程不进 IDE/终端，人只在 IM 出现 |

---

## 12. 决策记录（冻结日期 2026-08-07）

| 决策 | 定案 |
|---|---|
| D1 | goal 状态机加独立 `review` 态（与 blocked 对称：等人 vs 等子任务） |
| D2 | 验收判定（机器验证结果 + 卡点规则）在 `ReconcileOnRunEnd` 仲裁事务内，原子推进 |
| D3 | 验收策略是一等概念，**属域**；NL 意图 → 编译检查 → 卡点分流；编译在定义时刻、用户确认后冻结 |
| A1 | 执行工具 = openagent-cli serve --acp（协议连通已确认）；平台不依赖其任何特性 |
| A2 | 在哪个域就在哪干活；`agent.workdir_base` 删除；agent 执行的 goal 必须有域 |
| A3 | 与 A2 统一为 worktree 模型（域共享仓，goal 持 worktree+分支） |
| A4 | 交付自动：approve → 平台 merge + 复验 + push；失败回 review 标注 |
| A5 | 上下文恢复 = 同 worktree（文件态）+ transcript 重放（会话态）；session/resume 留作长驻路径 |
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
| 原则修正 | 平台能力不得依赖特定 agent 特性（--approver 等一律不用）——2026-08-07 对齐 |
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
| 决策2-14 | **M1 加 IM 最小通知**（goal 完成/卡点待审/失败推送）——解决"无人值守后审批触达"裂缝；M3 完整交互（审批卡/汇报/每日摘要/入站）。**通道：飞书（2026-08-08 定案）**，notify 按 adapter 模式实现，渠道可插拔 |
| 决策2-15 | **演进指标基线**：M0 建域记 metrics_baseline（测试数/覆盖率），M2 起展示趋势——自举证明的可量化第二层 |
| 决策2-16 | **任务验收强度**：M0 自举任务加"覆盖率提升 ≥ N%"约束（防空测试过关），演示结构化约束完整形态 |
| 决策2-17 | **处理器 run 形态**：run 表加 `run_kind`（worker/processor），与 worker run 同一调度；处理器 agent 建议独立配置 |
| 决策2-18 | **worktree 延迟分配**：第一个 run 开始前才创建，backlog 不占资源 |
| 决策3-1 | **验证环境准备 = setup 段（属验收策略）**：M3 实战暴露"干净 worktree 无依赖（npm build 失败）"。环境准备是验收策略的一等字段（processor 编译时输出，幂等命令；确认卡可见可改；平台顺序执行 setup→verify，setup 失败=run failed 归因环境）。平台不做技术栈自动检测（违反三角分离 + heuristic 脆弱） |
| 决策3-2 | **确认卡可编辑**：编译产物在冻结前可改（setup/verify/excludes 命令列表 + 完整 JSON）——兑现 §5.3"产物可见、可改、可审" |
| 决策3-3 | **提交排除属域（checks.excludes）**：平台提交 agent 改动时排除的路径由域声明（processor 从仓库 .gitignore/依赖目录编译 + 人确认），**平台零写死**——"仓库该忽略什么"是仓库的领地，平台硬编码 node_modules 等目录既追不上新依赖形式、又可能误排除用户故意跟踪的目录 |
| 决策3-4 | **issue 适配器多 provider（github/gitcode）**：Git 托管平台各说各话（API 形态、webhook 签名头、close 语义不同）——`internal/issue` 定义 Provider 接口（list/comment/close），域级 `issue_provider` 选实现；source_ref 带 provider 前缀；webhook 端点按 provider 分（X-Hub-Signature-256 / X-GitCode-Signature-256 / X-GitCode-Token）；轮询兜底不依赖任何平台特性 |
| 决策3-5 | **远程操作身份 bot 分离**：git_credentials 是平台的远程操作身份（issue 评论/close + git push 都以此账号出现——托管平台 API 硬约束：评论作者 = 认证 token 属主，无法伪装）。配置专用 agentwork-bot 账号的 token（GitHub/GitCode 一致），人的身份保持干净；commit 身份已由 git_identity 独立（"agentwork[bot]"） |
| 决策3-6 | **子 goal 机制休眠（2026-08-10 定案）**：代码与状态机（blocked/wake/wait-children）保留，但从 agent 引导（AGENTWORK.md、squad briefing、CLI usage）中移除子 goal 与 wait——协作统一走 mention（同 goal 协作 run），agent 创建的 goal 不再默认成为子 goal。理由：子 goal 服务"单 goal 内并行拆活"，两个前提（可靠拆分、合并冲突自修复）当前均未满足——leader 实测 wait 死锁（无子 goal 时永久 blocked，仅可 Cancel）；实际协作（写完→审查→审批）是顺序流，mention 全覆盖；愿景中的并行是平台层面多 goal（issue 轮询天然多 goal），已成立。恢复条件：出现真实并行拆活需求（如大 issue 拆活）+ 冲突自修复落地后，重新启用并补全子 goal 独立交付语义 |

---

## 13. 后置问题清册（记录在案，不阻塞）

- 远程 CI 集成（M0/M1 的验证 = 本地命令）
- 多 agent 并发操作同一域（分支冲突、merge 冲突的 agent 自修复）
- 汇报卡片的证据聚合智能化（LLM 摘要）
- 任务级策略覆盖的 NL 编译流程
- 验证产物漂移治理（仓新增技术栈 → 提示重新编译）
- 失败后的人工接手路径（failed goal 重开/改派 UI）
- 多域设置向导（NL → 编译 → 确认的交互形态）
- 备份/数据安全策略
- 成本追踪
- worktree 生命周期清理的数值（保留天数）
- 健康度学习引擎（连批 20 次建议删门；连拒 3 次建议收紧）
- squad 子 goal 独立合入 main，父 goal 只做协调汇总（方向已定，实现后置）——子 goal 机制休眠中（决策 3-6），激活时一并实现
- 审批时提示"review 期间有 N 条未执行 mention，是否执行"（决策 2-3 的代价缓解）
- 远程 CI 集成时配套"CI 红 → 自动 revert"回滚规则（决策 2-7）
- 域类型只实现 repo；文档/配置/知识库/backlog 等资产域后置
- IM 入站指令（@agentwork 建任务）——M3；Git 触发（webhook）——后置（闭环图 ① 的未来项）
- notify 渠道扩展（钉钉/Telegram/微信等）——飞书已定案为首个实现（决策 2-14），adapter 模式可插拔
- 候选队列 + 优先级 + 感知层（CI 红/依赖过期自动产生演进候选）——"全自动演进"的日常形态（决策 2-11）
- 自举部署闭环：合入 → 重新构建 → 重启 daemon（决策 2-12）
- 处理器 agent 的扩展用途：transcript 摘要、汇报生成、卡点健康度分析（决策 2-4 推论）
- git 凭据分权：agent 推分支与 deliver 推 main 分离（决策 2-13）
