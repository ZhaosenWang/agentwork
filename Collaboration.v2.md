# AgentWork 协作模型终稿（v2 · final）

- Status: **Approved**（已定稿，决策写入 DESIGN.md 6-1~6-10）
- 核心原则：**Goal 定义协作目标，Run 定义执行主体，Workspace 定义执行隔离，Worktree 定义代码隔离，Change 定义变更交付，Integration 定义变更进入 Owner 工作区的方式。**

---

# 1. 核心模型：8 个对象

```
Goal（长期状态，第一公民）
 ├── Owner（角色，不一定是 Agent——对 Goal 最终结果负责）
 ├── Run（owner | sub_goal | consult | review | verify）
 ├── Sub-goal（工作责任：assignee + verifier）
 ├── Workspace（run 级执行隔离）→ Worktree（代码隔离）
 ├── Change（跨 workspace 的变更载体，含 Revision）
 ├── Verification（sub-goal 质量判定）
 ├── Collaboration Event（audit / timeline）
 └── Conversation（评论 feed）
```

# 2. 四协作原语 + 权限矩阵

| 原语 | 语义 | 机制 |
|---|---|---|
| Comment（说） | 我告诉你一件事 | 纯评论，不触发 run、不改所有权 |
| Consult（问） | 我问你一个问题 | mention → 只读 guest run → 回答 → **创建 requester 的 successor run** |
| Handoff（接力） | 整个 Goal 换负责人 | 只终止 owner run；sub-goal runs 继续 |
| Sub-goal（做） | Owner 拆一块工作出去 | 新 SubGoal 实体，**不是 child goal**、不可递归 |

| 调用者 | Comment | Consult | Handoff | Create Sub-goal |
|---|---|---|---|---|
| Human | ✅ | ✅（mention） | ✅（HTTP assign） | ✅ |
| Goal Owner run | ✅ | ✅ | ✅ | ✅ |
| Sub-goal assignee run（worker） | ✅ | ✅ | ❌ | ❌ |
| Consult 应答 run（guest） | ✅（交答案） | ❌ | ❌ | ❌ |
| Review / Verify run | ✅ | ❌ | ❌ | ❌ |
| Squad Leader | ✅ | ✅ | ✅（代表 squad owner） | ✅ |

> Squad Leader 的权限**不是独立权限域**：源自现有语义 `goal.assignee=squad` 时 leader 是 owner 的代理（squad 表/leader 路由/审查 checkpoint 均为既有概念）。

> **不可递归三重保险**：schema 无 parent 列 + `create_sub_goal` 只接受 goal_id + 权限 owner/human/leader——模型层面消灭递归。

# 3. 执行模型：run 级 Workspace + per-run Worktree

- **Workspace 按 Run 实例化，不按 Goal**。布局：`~/.agentwork/repos/`（bare 共享仓）+ `~/.agentwork/runs/<runID>/`（per-run worktree）+ `proc/`
- **不变量：Workspace 出生时 = claim 时刻的 branch HEAD**。owner 单飞 + fresh workspace 使"workspace 落后 Goal HEAD"在 owner 身上不可能发生（goal 分支唯一写者就是那一个 owner run；sub-goal 写自己的分支；集成在 owner run 内进行）。跨 ref 的 git merge 天然处理 base 计算，无需人工同步步骤
- **Workspace 的 cleanliness 不是不变量，Workspace ownership 才是**：`runs/<runID>/` 路径即归属边界——crash recovery 后目录脏了无所谓，脏的就是这个 run 自己的产物；Workspace B 永不被 C 使用
- **串行化退化为**：owner run 单飞 + 每 sub-goal 至多一个 run；consult/verify run 只读、可与 owner 并行（读分支快照）。**同 agent 多 workspace 并行由 per-agent semaphore 控制——agent identity 与 workspace identity 是两个维度，禁止退回 per-agent 串行**
- **A5 恢复模型修订**：文件态 → 分支态。run 结束 `commitRunChanges`（checkpoint）；下一 run 从分支 HEAD 切新 workspace + transcript 重放。脏检查逃生门/归因机制退役；guest/verify 结束 `reset --hard` + `clean -fd`
- **Consult 只读的精确口径**：**domain read-only**——不能修改 goal/sub-goal/change 状态；workspace 允许 ephemeral 写（测试 cache 等），不产生 Deliverable/Change
- **术语统一（代码改名）**：`workspaces/`→`runs/`、`workspaceRoot()`→`runsRoot()`、`goalWorktreePath()`→`runWorktreePath()`、MCP `Executor.Workdir`→`Worktree`；`run.workdir` 列保留（通用词）

# 4. Change 与 Integration

```
Sub-goal Done → [验证] → Verified → Change Ready → Owner Integrate → Integrated
```

- **Change 是逻辑交付物；Revision 是它针对不同 Integration Base 的实现**：
  ```
  Change C
   ├── Revision 1: base=G1, head=C1
   └── Revision 2: base=G2, head=C2   （G2 = 含他人集成后的 Goal HEAD）
  ```
- **原子创建**：Change 与第一版 Revision 在**同一事务**落库——不存在"Change 无 Revision"状态（不需要 preparing 状态）；`Change Ready` 永远对应一个已持久化的 Revision
- 状态机 4 态：`Ready → Integrating → Integrated`；`↘ Conflict →（assignee 新 workspace 从当前 Goal HEAD 切出，重应用自己变更）→ Revision 2 → Ready`
- **Integration 的执行主体是 Owner Run**：Coordinator 只负责创建/恢复 owner run，**永远不直接执行 git 操作**；owner run 内 load Ready Changes → merge/cherry-pick → conflict → Change Conflict / success → Change Integrated
- 非代码型 sub-goal 是**正式模型**（Deliverable = Evidence/Report，走评论 feed）：ready 判定看 `sub_goal.status == Verified`，不看 has Change

# 5. Goal Coordinator：Event 驱动 Reconcile，不是业务决策

**Event 只是 wakeup hint；DB 是唯一真相。**

```
Event Bus
   │  wakeup hint
   ▼
Goal Coordinator ──► DB Transaction
                       │
                       ▼
                   ReconcileGoal(goalID)
                       │  read authoritative DB state
                       ├── deriveOwnerAttention()
                       ├── attention ≠ empty && ownerIdle → conditional enqueue owner run
                       ├── Sub-goal Rejected → quality_iteration++ → conditional enqueue assignee run
                       └── （人 owner → 落 attention + IM 通知，不 spawn）
                       ▼
                 Emit Events（commit 后）
```

- **OwnerAttention 是派生状态**（不为空才值得叫醒 owner）：
  ```
  need_integration   = ∃ Change(ready|conflict) 或 ∃ 未集成且 Verified 的 sub-goal
  need_recovery      = ∃ Sub-goal Failed / ∃ Change conflict 反复 / 集成异常
  need_user_action   = handoff loop 审停等
  ```
  硬规则：**owner run 的创建只能由 Coordinator → deriveOwnerAttention → conditional enqueue 产生**（P0.5 统一 spawn 入口，禁止任何 handler/工具直接 spawn）
- **Reconcile 幂等**：执行 100 次最终状态相同。所有关键转移 conditional transition（`WHERE status=expected` / `INSERT ... WHERE NOT EXISTS (同 goal 且 role=owner 且 queued/running)`）
- **Latch 双触发**：`attention != empty && ownerIdle` 是状态条件不是一次性事件——**Sub-goal/Change state changed 与 run.terminal 两个方向都必须触发 Reconcile**。硬规则：*任何可能改变"Goal 是否需要 Owner attention"判定的状态变更，都必须触发 Goal Reconcile*
- **`run.terminal {run_id, goal_id, status}` 事件**（Finish 统一发布），Coordinator 订阅它 + sub_goal.* + change.*
- **Owner Resume Context 是索引不是数据库 dump**：goal 摘要 + sub-goal 清单（✓/✗）+ Change 状态 + 验证摘要 + Attention 项；详情按需 `agentwork_get_sub_goal` / `get_change` / `get_verification` 展开
- run 状态机不加 Suspended/Waiting：suspend = run 自然结束；**恢复统一叫 successor run**（"Consult 完成后创建 requester 的 successor run"，不存在 resume 原 run）
- run 内事件（tool result）不走 Coordinator——那是 agent 自己的循环

# 6. Verifier 机制

- Sub-goal 三角色：Assignee（做）/ Verifier（验）/ Goal Owner（集成 + Final Verify）。**Assignee ≠ Verifier**
- **Verifier 分层——默认机器当 Verifier**：
  - 默认：sub-goal run 结束 → 域验证命令跑在 sub-goal workspace → 通过 = Verified → Change Ready；失败 = 机器验证失败
  - 可选：指定 `verifier_id`（agent）→ Verifier Run（只读、复用 review run 机制、多轮）+ 结构化判定工具 `agentwork_verify_sub_goal(verdict, summary, evidence)`
  - **verifier_type ∈ {machine, agent}，无 human verifier**（human 质量把关在 goal 卡点；不为未来设计用不上的抽象）
- **两层失败语义，两个计数，绝不混**：
  | 路径 | 语义 | 计数 | 耗尽 |
  |---|---|---|---|
  | 机器验证失败 | 执行/验证尝试失败，不是被人否决 | `execution_attempt`（≤3） | Sub-goal = Failed（报 owner） |
  | Verifier Reject | 成果被质量责任人拒绝 | `quality_iteration`（不设硬上限，镜像 goal.human_iterations） | Sub-goal 回 Running → assignee 新 run |
- **Verifier 只产生判定，不启动下一轮**：persist verification_result(rejected) → SubGoal=Rejected → event → **ReconcileSubGoal** → quality_iteration++ → conditional enqueue assignee run（P0.5 一致）
- **Verify Run 只读强化**：可以产生 Evidence，不能产生 Deliverable Change——测试/cache 走 workspace ephemeral 最终 discard；不能 commit、不能改 Change
- `verification_result` 表：每轮一条。轮次 ≠ run.attempt
- Sub-goal 状态机：`Pending → Running → Done → Verifying → Verified | Rejected → Running`（+ Cancelled / Failed）
- 两道门不合并：Sub-goal Verification → Integration → Goal Final Verification
- **Final Verification 触发 = Owner 决定**（结束 turn）：run 完成 → 域验证 → gates → 人审。**Coordinator 不自动进入**（它不理解"业务集成完成"）

# 7. Review 与 Verify（正式定义，二者不同）

| | Review run | Verify run |
|---|---|---|
| 层级 | goal 级 | sub-goal 级 |
| 触发 | 平台自动（goal 进 review 时拉 squad role=reviewer 成员，决策 4-4） | sub-goal 指定 verifier_id 时，Done 后派发 |
| 性质 | 主观、**无判定权**——意见进评论供人审批参考 | 有判定权——verdict 经结构化工具落 verification_result |
| 只读 | 是（ephemeral 写允许） | 是（同上） |
| 产物 | 评论 | verification_result + summary + evidence |
| 失败 | 意见仍在（guest 失败留痕，决策 4-3） | rejected → quality_iteration++ |

# 8. Handoff 语义

- Handoff 只换 Goal Owner；**既有 sub-goal 的 assignee/verifier/run/change 全部不变**；新 owner 获得管理权，不自动成为任何 sub-goal 的 assignee
- **Handoff 是 Goal Owner 的变化，不是 Run 生命周期的新状态**：`run.status` 保持五态（queued|running|completed|failed|cancelled），`cancel_reason=handed_off` 承担语义（**撤销决策 5-5 的 handed_off 状态**）；handoff 事件入 handoff_event 表
- **forced handoff（MVP 唯一形态）**：kill 旧 owner run → **不代提交 WIP**（mid-git-op 代提交不安全）→ WIP 留在 `runs/<old-runID>/`（取证）→ 新 owner 从 branch HEAD 切新 workspace。AGENTWORK.md 引导"handoff 前随手 commit"。graceful handoff（信号→checkpoint→terminal）后置
- **Deliver 只等 owner run**：`deliverWaitForOwnerRuns(goalID)`——sub-goal run 不碰 goal 分支，不阻塞交付
- Handoff Snapshot 不建持久化实体：新 owner run 的 prompt 注入即 snapshot
- Handoff Cycle 保持现状（4 警告 / 8 审停，不判 failed）

# 9. Consult 恢复

- 恢复目标 = `consult_request.requester_run_id` 的 **successor run**（按 requester run 角色路由：owner run → owner successor；sub-goal run → 该 sub-goal 的 successor）。**禁止 GoalID → Owner 作为通用恢复机制**
- guest 回答落 feed + response_comment_id 回填；mention pingpong 守卫（4/8）口径不变

# 10. Goal 级 Cancel / Delete / Reopen（级联语义）

| 操作 | 级联 | 保留 |
|---|---|---|
| **Cancel** | 所有 non-terminal sub-goal → Cancelled；所有 active runs → 终止；pending integration/verification 取消 | Change/VerificationResult **历史保留** |
| **Delete** | 软删除优先：`status=deleted` + 异步 GC（workspace/logs/artifacts）；物理删除则 sub_goal/run/change/verification/workspace 全级联，**Run 与 Change 不得留孤儿** | — |
| **Reopen** | **不复活旧 sub-goal**（保持各自终态）；owner 新 run 拿到"重新评估当前 Goal 状态"上下文，重新拆活 | 历史语义不变 |

- **Human owner 的 attention 持久化**：`goal.attention` 列（reconcile 事务内 conditional 更新）——UI 徽标与 IM 通知去重的依据；IM 只是通道。all-ready 时 human owner 不 spawn，走 attention + 通知
- **required 语义**：当前版本**所有 sub-goal 均 required**（不建列，文档写死）。ready 判定精确定义：`every(sub_goal, status ∈ {Verified, Cancelled})` 且 ∃ 待集成项；`Failed` → need_recovery attention

# 11. 事件清单

```
sub_goal.created / verified / rejected / cancelled / failed
change.ready / integrated / conflict
verification.completed
run.terminal {run_id, goal_id, status}
```

# 12. 数据模型

| 变更 | 说明 |
|---|---|
| `sub_goal` 表（新） | `id, goal_id, title, description, assignee_id, verifier_id(''=机器), status, execution_attempt, quality_iteration, created_at`——**无 run_id 列**，关系真相 = `run.sub_goal_id → sub_goal.id`（1:N），当前 run 现查 |
| `change` 表（新） | `id, goal_id, sub_goal_id, status(ready/integrating/integrated/conflict), created_at`——与第一版 Revision **同事务原子创建** |
| `change_revision` 表（新） | `id, change_id, seq, base_ref, head_ref, created_at` |
| `verification_result` 表（新） | `id, goal_id, sub_goal_id, verifier_run_id, status, summary, evidence, created_at` |
| `run` 表 | 加 `sub_goal_id`；`role` 加 `subgoal`/`verify`；**`attempt` 降级为实例序号**（展示/prompt 上下文），重试判定权威在实体计数；status 五态（handed_off 撤销） |
| `goal` 表 | 加 `execution_attempt`（机器重试计数，权威）、`attention`（派生状态持久化）；`parent_id` 子 goal 用法退役 |
| `consult_request` | 已有（requester_run_id 即恢复锚点） |
| 删除 | blocked 路径、wake_count、WaitChildren/wakeParentIfReadyInTx/goal_wait |
| 新增 MCP 工具 | `agentwork_cancel_sub_goal`（reassign 用 cancel + create 替代）、`agentwork_verify_sub_goal`、`agentwork_get_sub_goal` / `get_change` / `get_verification` |

# 13. Runtime Invariants（最终 16 条）

1. **Event 不是真相**——wakeup hint；DB = source of truth
2. **Reconcile 幂等**——执行 100 次最终状态相同
3. **关键状态转移全部 conditional transition**——`WHERE current_status = expected`
4. **Owner 不等待 Sub-goal**——suspend = run 结束；sub-goal 完成 → Event → Reconcile → successor run
5. **run.terminal 必须 Reconcile**——Sub-goal/Change changed 与 run.terminal 两条边都必须存在（防 Latch）
6. **Sub-goal Done ≠ Verified**——只有 Verified 计入 All Required Sub-goals Ready
7. **Assignee ≠ Verifier**——做/验/集成分离；默认 Verifier = 机器（域验证），可委托 agent（machine|agent，无 human）
8. **Consult 恢复 requester_run_id**——创建 requester 的 successor run；永远不用 goal_id → owner
9. **Handoff 只改变 Owner**——sub-goal 的 assignee/verifier/run/change 全部不变
10. **Workspace 属于 Run**——Run → Workspace → Worktree；路径即归属；出生 = claim 时刻 branch HEAD
11. **Change 是跨 Workspace 的交付载体**——禁直接 copy 文件
12. **Change Revision 必须绑定 Integration Base**——base_ref/head_ref；conflict 后：当前 Goal HEAD + old Change = new Revision
13. **Owner run 创建只经 Coordinator**——deriveOwnerAttention → conditional enqueue（P0.5）；Verifier 判定不直接 spawn
14. **Coordinator 不执行 git**——Integration/验证只在 run 内发生
15. **Change Ready ⇔ 已持久化 Revision**——原子创建，不存在无 Revision 的 Ready
16. **Consult 只读 = domain read-only**——workspace ephemeral 写允许，不产生 Deliverable/Change

# 14. 代码改造点清单（按优先级）

| 优先级 | 内容 |
|---|---|
| **P0** | Event≠Truth（ReconcileGoal）、run.terminal→Reconcile（Latch）、deliver 只等 owner run、Conflict Revision+Base、Goal cancel/delete/reopen 级联、两层失败计数、handed_off 回滚为 cancel_reason |
| **P0.5** | 统一 spawn 入口（禁止 handler 直发 owner run） |
| **P1** | crash recovery workspace ownership、cancel_sub_goal 工具、human owner attention 持久化 + IM、verify run 只读 |
| **P2** | 无代码 sub-goal、resume context 索引化 + get_* 工具、fan-out ≤20 active（历史不限）、notify 口径、同 agent 多 workspace、术语改名 |

阶段：**文档**（DESIGN.md 6-x + 本文件）→ **实体层**（schema 新表新列）→ **workspace 层**（run 级 worktree + Claim + 恢复 + 改名）→ **sub-goal 流程** → **Coordinator**（含 P0.5）→ **Verifier** → **UI + 测试**。每阶段独立编译 + 全绿。

# 15. 验收场景

- **E2E 并行**：owner 拆 2 个 sub-goal → 3 run 并行 → 2 Change Ready → owner 被 Reconcile 唤醒 → integrate → goal 验证 → 卡点 → deliver
- **Latch**：sub-goals 在 owner 运行中全 ready → owner run terminal 后 Reconcile 唤醒（不丢）
- **Handoff 中途**：A suspend → handoff E → B/C 继续 → verified → E 被唤醒 integrate → done
- **Consult 于 sub-goal**：B consult C → C 回答 → **B 的 successor run**，A 不被唤醒
- **Conflict 多轮**：B/C 同文件 → integrate C 冲突 → C 从新 Goal HEAD 重应用 → Revision 2 → 再 integrate 成功
- **机器验证 vs Verifier reject**：机器失败 3 次 → Failed 报 owner；QA reject 2 轮 → 第三轮 Verified（quality_iteration=2）
- **Goal Cancel**：级联停 sub-goals/run，Change/Verification 历史保留；Reopen 不复活旧 sub-goal
- **Human owner**：all-ready → goal.attention 落库 + IM 通知，无 spawn
