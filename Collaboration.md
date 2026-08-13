# AgentWork Collaboration Mechanism
# 协作机制优化与修复设计文档

- Status: **Adopted（2026-08-13 全盘采纳）**，冻结决策记录在 DESIGN.md 决策 5-1 ~ 5-10
- Version: 1.0
- Scope: AgentWork Collaboration / Goal / Run / Comment / Multi-Agent
- Purpose: 指导现有代码优化、修复及后续协作能力扩展

> **实现偏差注记**（相对本文档的最终落地差异，以 DESIGN.md 决策 5-x 为准）：
> 1. §8「Mention 权限范围」的 workspace/squad 级访问过滤**未做**——单机单用户模型下所有 agent 可被 consult（与既有 trust boundary 一致），留作后置
> 2. §21「Handoff Cycle」阈值具体化为 **4/8**：≥4 次写系统评论警告；≥8 次 goal 进 review 停靠（复用 ResolveReview，人 approve 继续/reject 驳回）
> 3. §11 guest run「默认不能改文件」平台强制方式：run 结束丢弃 guest 窗口期新产生的未提交改动（entry 脏路径快照求差）；guest **直接 git commit** 进分支的改动不 revert（无法无损还原 entry 状态），检测到写系统评论提醒，交付前人工可见
> 4. §40 `handed_off` 状态替代原 handoff→cancelled 路径；`cancel_reason=handoff` 保留，notify/收敛规则与 cancelled+handoff 相同。**已再被决策 6-6 撤销**：run.status 回五态，`cancel_reason=handed_off` 承担语义
> 5. §27 工具名落地为 `comment_goal` / `consult_agent` / `handoff_goal` / `create_sub_goal` / `goal_wait`；旧的 goal_comment/goal_assign 移除（决策 5-2）。**经决策 6-10 修订**：goal_wait 随 blocked/wait 退役移除；工具面扩充 cancel_sub_goal / verify_sub_goal / integrate_change / get_change / get_sub_goal / get_verification
> 6. §10「Answer → Requester」落地为 consult 闭环自动恢复（决策 5-8）：guest run completed 且 requester 仍是 owner 且 goal active → 平台自动 enqueue requester 的下一 run
> 7. §6「Guest Run → Agent 不允许」落地：agent 作者评论中的 mention 只在作者是 owner run 时 dispatch；human 评论 mention 不受限（§7 human 语义）

---

# 1. 背景

AgentWork 当前已经具备：

- Agent
- Goal
- Run
- Comment
- Mention
- Guest Run
- Handoff / Assign
- Sub-goal / Child Goal
- Squad / Leader

但目前这些机制之间的语义边界不够清晰，导致：

1. `mention` 同时承担咨询、委派、任务分配等语义
2. Agent 与 Human 的 mention 行为容易混淆
3. Handoff 与 Sub-goal 边界不清晰
4. Leader 与 Goal Owner 的权限容易混淆
5. Guest Run 与正常 Worker Run 的职责不够明确
6. Handoff 后 Run 生命周期容易出现并发问题
7. Multi-Agent 协作容易形成 Agent Ping-Pong
8. UI 中不同协作行为都表现为 Comment，导致用户无法理解当前发生了什么
9. 后续 Kanban / Goal Tree / Approval / Replay / Audit 很难建立稳定语义

本设计不推翻现有 AgentWork 架构。

目标是：

> **通过统一协作语义、权限模型、Run 生命周期和事件模型，修复现有代码中的协作混乱问题。**

---

# 2. 核心设计原则

AgentWork 协作最终只保留四种基本行为：

| 行为 | 语义 |
|---|---|
| Comment | 告诉别人一件事 |
| Consult | 向其他 Agent 请求信息 / 判断 / 建议 |
| Handoff | 转移当前 Goal 的所有权 |
| Sub-goal | 创建一个独立工作任务 |

最重要的定义：

```text
Comment  = 说
Consult  = 问
Handoff  = 接力
Sub-goal = 拆活
其中：
@mention → Consult
而：
Handoff → Ownership Transition
Sub-goal → New Goal
必须严格区分。

⸻

3. 三种协作流
整个 AgentWork Collaboration 可以抽象成三个 Flow：
                  Collaboration
                       |
          +------------+------------+
          |            |            |
          v            v            v
      Information   Ownership     Work
         Flow         Flow         Flow
          |            |            |
          v            v            v
       Consult       Handoff      Sub-goal
          |            |            |
          v            v            v
       @mention     Owner Change   Child Goal
因此：
Consult
解决：
“我需要你告诉我一些东西。”
Handoff
解决：
“我不再负责这个 Goal，你接着负责。”
Sub-goal
解决：
“这个 Goal 里面有一块独立工作，你来负责。”

⸻

4. Comment
4.1 定义
Comment 是普通通信。
例如：
Backend Agent:

我发现问题出在 OAuth callback。
Comment 不产生：
Goal
Run
Owner Change
Handoff
Sub-goal

⸻

5. Mention / Consult
5.1 核心语义
AgentWork 中：
@Agent
统一表示：
Consult。
例如：
@Backend 这个 API 的鉴权逻辑应该怎么处理？
语义：
Consult Backend
而不是：
Delegate Backend
Handoff Backend
Create Goal

⸻

6. Agent Mention
任何拥有 Active Run 的 Agent 都可以 Consult 其他 Agent。
例如：
Goal Owner = Frontend

Frontend Run
    |
    +---- @Backend
允许。
普通 Worker 也可以：
Worker A
    |
    +---- @Worker B
允许。
因此：
Worker → Worker       YES
Worker → Leader       YES
Leader → Worker       YES
Leader → Leader       YES
不允许：
Guest Run → Agent
No Active Run → Agent

⸻

7. Human Mention
Human 可以在 Goal 评论区：
@Backend
唤起任意当前 Goal 可访问的 Agent。
例如：
User:

@Backend 帮我看一下这个 OAuth 问题。
系统：
Comment
   |
   +-- Mention
          |
          v
     Consult Request
          |
          v
      Backend
          |
          v
     Guest Run
Human Mention 与 Agent Mention 使用同一套 Consult 机制。

⸻

8. Mention 的权限范围
“任意 Agent”不是指系统中的任意 Agent。
Human 可以 mention：
当前 Workspace / Squad / Goal Context 中具有访问权限的 Agent。
例如：
Goal
├── Frontend
├── Backend
├── QA
├── Security
└── DevOps
Human 可以：
@Frontend
@Backend
@QA
@Security
@DevOps
但不能随意唤醒无权限的 Agent。

⸻

9. Mention 不改变任务语义
以下两条评论：
@Backend 这个 API 应该怎么实现？
和：
@Backend 帮我把这个 API 修掉。
在 Comment 层都只是：
Comment + Mention
系统不应该根据自然语言直接改变 Goal Owner 或创建 Child Goal。
如果 Agent 判断需要实际工作，应主动调用：
create_sub_goal
如果需要接管当前 Goal，应调用：
handoff_goal

⸻

10. Consult 执行模型
Consult 产生 Guest Run。
Requester Run
       |
       | consult
       v
Consult Request
       |
       v
Guest Run
       |
       v
Answer
       |
       +------> Requester
       |
       +------> Goal Conversation
Guest Agent 的回答应该直接回复：
发起 Consult 的 Agent / Human。
而不是默认回复 Leader。

⸻

11. Guest Run
Guest Run 是：
被当前协作上下文临时唤起，为其他 Agent 提供信息或判断的执行实例。
Guest Run 可以：
阅读 Goal Context
阅读相关 Comment
分析代码
查看 Workspace
执行必要的只读操作
提供建议
返回分析结果
默认不能：
改变 Goal Owner
完成 Parent Goal
代表 Parent Owner 执行 Handoff
无限创建 Sub-goal
改变 Parent Goal 生命周期
核心规则：
Guest Run != Owner Run
以及：
Guest Run 完成
    !=
Goal 完成

⸻

12. Consult 数据模型
建议统一为：
ConsultRequest
├── id
├── goal_id
├── requester_agent_id
├── requester_run_id
├── target_agent_id
├── trigger_comment_id
├── guest_run_id
├── response_comment_id
└── created_at
完整链路：
Comment
   |
   v
Mention
   |
   v
ConsultRequest
   |
   v
Guest Run
   |
   v
Response Comment

⸻

13. Handoff
13.1 定义
Handoff 是：
当前 Goal Owner 将当前 Goal 的所有权转移给另一个 Agent。
例如：
Goal #100
Owner = Frontend

Frontend
    |
    | handoff
    v
Backend

Goal #100
Owner = Backend
Handoff 不创建新的 Goal。

⸻

14. Handoff 权限
核心规则：
只有当前 Goal Owner 可以 Handoff。
例如：
Goal Owner = A
那么：
A → B       YES
C → B       NO
D → B       NO
Leader 并不天然拥有所有 Goal 的 Handoff 权限。

⸻

15. Leader 的特殊情况
如果：
Goal Owner = Squad
那么 Squad Leader 可以代表 Squad Owner 进行 Handoff。
因此：
Individual Goal
    |
    v
Individual Owner
    |
    +-- Handoff


Squad Goal
    |
    v
Squad Owner
    |
    v
Squad Leader
    |
    +-- Handoff
不建立：
Leader 可以 Handoff 任意 Goal
这种全局权限。

⸻

16. Handoff 是否可以回来
可以。
Handoff 是可逆的 Ownership Transition。
例如：
A
 |
 | Handoff
 v
B
 |
 | Handoff
 v
A
合法。
但必须产生完整的 Handoff History。

⸻

17. Handoff 与 Consult 的区别
以下两个行为必须严格区分：
B:
@A 你觉得这个问题应该怎么处理？
这是：
Consult
不会改变 Owner。
而：
B:
handoff Goal → A
表示：
B 不再负责当前 Goal，A 重新成为 Owner。

⸻

18. Handoff Run 生命周期
假设：
Goal #100
Owner = A

Run A-001 = running
A Handoff 给 B：
handoff Goal #100 → B
必须完成：
1. Validate A is current Owner
2. Validate B is valid target
3. Create Handoff Event
4. Change Goal Owner A → B
5. Terminate A-001
6. Create B-002
7. Start B-002
最终：
Goal #100
Owner = B

A-001
   |
   +-- handed_off

B-002
   |
   +-- running

⸻

19. Handoff 原子性
Handoff 必须通过统一 Service / Transaction 完成。
不要允许业务代码直接：
goal.owner = B
然后另一个地方：
run.cancel()
再另一个地方：
createRun()
这会产生：
Goal Owner = B
A Run = running
B Run = running
导致一个 Goal 同时存在多个 Owner Run。
因此应该提供唯一入口：
GoalService.Handoff(
    ctx,
    goalID,
    fromAgentID,
    toAgentID,
    reason,
)

⸻

20. Handoff Event
每次 Handoff 必须记录：
HandoffEvent
├── id
├── goal_id
├── from_agent_id
├── to_agent_id
├── from_run_id
├── to_run_id
├── reason
└── created_at
示例：
08:10
A started Goal

08:30
A → B
Handoff

Reason:
后续工作主要是 Backend implementation

08:31
B started

09:10
B → A
Handoff

Reason:
Backend changes completed

⸻

21. Handoff Cycle
允许：
A → B
B → A
但是需要检测：
A → B → A → B → A
这种 Ownership Ping-Pong。
不能简单：
handoff_count > N
    |
    v
Goal Failed
因为：
Handoff Cycle 是协作异常，不等于业务 Goal 失败。
推荐：
Cycle Detected
     |
     +-- Block Goal
     |
     +-- Request Human Approval

⸻

22. Sub-goal
> **Superseded（决策 6-1）**：SubGoal 已重定义为独立实体——不是 child goal（不可递归、依附 goal 生命周期、assignee/verifier 分离、无独立交付），完整模型见 Collaboration.v2.md。
22.1 定义
Sub-goal 表示：
当前 Goal 内的一块独立工作被拆出来，形成一个新的 Goal。
例如：
Goal #100
修复 OAuth

    ├── Goal #101
    │   Backend
    │
    └── Goal #102
        Frontend
Sub-goal 是：
Work Delegation

⸻

23. Handoff 与 Sub-goal
必须严格区分。
Handoff
同一个 Goal
Owner 改变
Goal #100
A → B
Sub-goal
创建新的 Goal
Parent Goal 不改变 Owner
Goal #100
    |
    +-- Goal #101
         Owner = B
一句话：
Handoff  = 接力
Sub-goal = 拆活

⸻

24. Sub-goal 权限
允许：
Current Goal Owner
Squad Leader（代表 Squad Owner）
Human
不允许：
Guest Run
普通非 Owner Agent

⸻

25. Sub-goal 生命周期
Child Goal 必须拥有独立生命周期：
Child Goal
├── Owner
├── Run
├── Status
├── Verification
├── Workspace
└── Completion
Parent Goal：
Parent
   |
   +-- Child A
   +-- Child B
   +-- Child C
Child 完成后：
Child Complete
      |
      v
Parent Resume / Re-evaluate
不要自动认为：
所有 Child Complete
      =
Parent Complete
Parent 仍然必须自己完成最终验证。

⸻

26. Collaboration 权限矩阵
Actor
Comment
Consult
Handoff
Sub-goal
Human
✅
✅
✅
✅
Goal Owner
✅
✅
✅
✅
Squad Leader
✅
✅
代表 Squad Owner
✅
普通 Worker
✅
✅
❌
❌
Guest Run
受限
❌
❌
❌

⸻

27. Collaboration API
Agent Runtime / MCP 层不要暴露一个含义模糊的：
mention
建议提供：
comment_goal
consult_agent
handoff_goal
create_sub_goal

⸻

27.1 consult_agent
{
  "goal_id": "goal-123",
  "agent_id": "backend",
  "question": "这个 API 的鉴权逻辑应该怎么处理？"
}

⸻

27.2 handoff_goal
{
  "goal_id": "goal-123",
  "agent_id": "backend",
  "reason": "后续主要是后端实现"
}

⸻

27.3 create_sub_goal
{
  "parent_goal_id": "goal-123",
  "title": "实现 OAuth callback",
  "description": "实现 callback state 验证",
  "assignee": "backend"
}

⸻

28. Agent 决策规则
Agent 应按照以下规则选择协作方式。
需要别人提供知识 / 判断？
        |
        v
     Consult
        |
        v
     @Agent
自己不再适合继续当前 Goal？
        |
        v
     Handoff
        |
        v
Goal Owner → Other Agent
发现一个独立工作？
        |
        v
    Sub-goal
        |
        v
Create Child Goal
只是同步信息？
        |
        v
     Comment

⸻

29. UI 表达
四种行为不能全部显示成普通 Comment。
Comment
💬 Backend Agent

我已经找到问题。
Consult
💬 Consultation

Frontend → @Backend

这个 API 应该如何鉴权？
Handoff
🔄 Handoff

Frontend Agent
      ↓
Backend Agent

Goal #100
同时 Goal Card：
Owner: Backend
Sub-goal
➕ Sub-goal

Goal #101
Fix OAuth UI

Owner: Frontend
并进入 Goal Tree / Kanban。

⸻

30. Goal Timeline
推荐 Goal Timeline：
08:00
A started Goal

08:15
💬 A consulted B

08:16
B answered

08:25
🔄 A handed off Goal to C

08:26
C started Goal

08:40
➕ C created Sub-goal #102

09:10
✓ Sub-goal #102 completed

09:15
🔄 C handed off Goal back to A

09:16
A resumed
这会成为 AgentWork 最重要的协作可视化之一。

⸻

31. Collaboration Event
建议将协作行为统一记录为 Event。
CollaborationEvent
├── id
├── type
├── goal_id
├── actor_agent_id
├── target_agent_id
├── source_run_id
├── target_run_id
├── parent_goal_id
├── child_goal_id
├── reason
└── created_at
Type：
consult
handoff
delegate
其中：
delegate = create_sub_goal

⸻

32. 核心 Invariants
> **Superseded**：v2 终稿的 16 条 Runtime Invariants（Collaboration.v2.md §13）取代本节 10 条。
这些规则必须成为 Runtime 硬约束。
Invariant 1
A Goal has exactly one current Owner.
Invariant 2
Only current Owner can Handoff.
Invariant 3
Consult never changes Goal Owner.
Invariant 4
Handoff does not create a new Goal.
Invariant 5
Sub-goal always creates a new Goal.
Invariant 6
Guest Run cannot complete the parent Goal.
Invariant 7
A Goal has at most one active Owner Run.
Invariant 8
Guest Run != Owner Run.
Invariant 9
Handoff terminates the previous Owner Run.
Invariant 10
Sub-goal has an independent lifecycle.

⸻

33. 现有代码优化策略
本次优化不建议重写 Collaboration。
采用：
Semantic Refactor
+
State Machine Fix
+
Permission Fix
+
Event Unification

⸻

34. 第一阶段：统一语义
首先明确：
mention
    ↓
consult
assign / transfer
    ↓
handoff
child goal
    ↓
sub-goal
避免代码中出现：
mention → maybe assign
mention → maybe create goal
mention → maybe handoff

⸻

35. 第二阶段：统一权限
所有操作进入统一 Policy：
CanComment(actor, goal)
CanConsult(actor, goal, target)
CanHandoff(actor, goal, target)
CanCreateSubGoal(actor, goal, assignee)
不要在多个 Handler 中分别判断：
if leader
if owner
if squad
...

⸻

36. 第三阶段：统一 Handoff
建立：
GoalService.Handoff(...)
所有 Owner 转移必须经过这里。
禁止：
goal.OwnerID = target
直接修改。

⸻

37. 第四阶段：统一 Consult
所有 Mention：
Comment
   ↓
Mention Parser
   ↓
ConsultRequest
   ↓
Guest Run
不要在 Mention Handler 中直接：
create arbitrary Agent Run
必须明确：
RunType = Guest

⸻

38. 第五阶段：Sub-goal
所有 Child Goal 创建统一进入：
GoalService.CreateSubGoal(...)
必须自动建立：
parent_goal_id
以及：
child_goal_id
形成 Goal Tree。

⸻

39. Run Type
建议明确 Run Type：
OwnerRun
GuestRun
如果未来增加更多 Run 类型，再扩展。
例如：
OwnerRun
GuestRun
ReviewerRun
PlannerRun
但是：
OwnerRun
必须是唯一拥有 Goal Execution Authority 的 Run。

⸻

40. Run State
> **Superseded（决策 6-6）**：`handed_off` 状态已撤销——handoff 是 Goal 层事件，不是 run 生命周期状态；run.status 保持五态，`cancel_reason=handed_off` 承担语义。
建议明确：
pending
running
waiting
completed
failed
cancelled
handed_off
Handoff 后：
old Run
    |
    v
handed_off
而不是：
failed
因为：
Handoff 不是执行失败。

⸻

41. Goal State 与 Run State 分离
必须避免：
Run completed
    =
Goal completed
尤其 Guest Run。
正确关系：
Guest Run completed
    |
    v
Consult completed

Owner Run completed
    |
    v
Goal may be completed
最终 Goal 是否完成必须由 Goal Owner / Runtime 验证。

⸻

42. Human Approval
涉及以下操作时，可以进入 Approval：
Handoff
Sub-goal creation
High-risk Agent invocation
External side effect
Deployment
但：
Consult
通常不应该要求 Human Approval。
因为 Consult 本身只是信息请求。

⸻

43. Anti-Patterns
禁止：
@Agent = assign task
禁止：
@Agent = handoff
禁止：
Guest Run = Worker Run
禁止：
Leader = every Goal Owner
禁止：
Handoff = Goal Failure
禁止：
Child Goal Complete = Parent Complete
禁止：
多个 Active Owner Run
禁止：
Guest Agent 无限创建 Sub-goal

⸻

44. 测试计划
Consult
TestConsultCreatesGuestRun
TestConsultDoesNotChangeOwner
TestConsultDoesNotChangeGoalState
TestConsultResponseGoesToRequester
TestHumanMentionCreatesConsult
TestAgentMentionCreatesConsult
TestUnauthorizedMentionRejected

⸻

Handoff
TestOnlyOwnerCanHandoff
TestSquadLeaderCanHandoffSquadGoal
TestWorkerCannotHandoff
TestHandoffChangesOwner
TestHandoffTerminatesOldRun
TestHandoffCreatesNewRun
TestHandoffCanReturn
TestHandoffHistoryRecorded
TestHandoffCycleDetected
TestHandoffIsNotGoalFailure

⸻

Sub-goal
TestOwnerCanCreateSubGoal
TestSquadLeaderCanCreateSubGoal
TestGuestCannotCreateSubGoal
TestWorkerCannotCreateSubGoal
TestSubGoalHasParent
TestSubGoalHasIndependentOwner
TestSubGoalHasIndependentRun
TestParentDoesNotChangeOwner
TestParentResumesAfterChildCompletion

⸻

45. Migration Plan
Phase 1
只修语义，不改 UI。
mention → consult
assign → handoff
child goal → sub-goal

⸻

Phase 2
增加权限检查：
CanConsult
CanHandoff
CanCreateSubGoal

⸻

Phase 3
统一 Run 生命周期。
重点修复：
Handoff
    ↓
Old Run termination
    ↓
New Run creation

⸻

Phase 4
统一 Collaboration Event。
建立：
ConsultEvent
HandoffEvent
SubGoalEvent

⸻

Phase 5
UI 改造：
@mention
    → Consultation

Handoff
    → System Event

Sub-goal
    → Goal Card / Goal Tree

⸻

46. 最终模型
AgentWork 的协作机制最终应该非常简单：
                     Collaboration

                           |
          +----------------+----------------+
          |                |                |
          v                v                v
       Consult          Handoff          Sub-goal
          |                |                |
          v                v                v
       @mention       Owner Transfer     New Goal
          |                |                |
          v                v                v
      Guest Run        New Owner        Child Run
人类：
@Agent
也是：
Consult
Agent：
@Agent
也是：
Consult
只有明确调用：
handoff_goal
才发生 Owner 转移。
只有明确调用：
create_sub_goal
才创建新的工作。

⸻

47. 最终认知模型
整个 AgentWork 协作机制可以压缩成四句话：
Comment：我告诉你。
Consult：我问你。
Handoff：我把这个 Goal 交给你。
Sub-goal：我把这个独立工作拆给你。
对应：
Comment
   ↓
Information

Consult
   ↓
Information Request

Handoff
   ↓
Ownership Transfer

Sub-goal
   ↓
Work Delegation

⸻

48. 最终设计决策
本设计确定以下规则：
@mention 的唯一协议语义是 Consult
Human 可以 Mention 当前有权限访问的任意 Agent
Active Agent 可以 Mention 其他 Agent
Mention 不改变 Goal Owner
Mention 不创建 Sub-goal
Mention 不执行 Handoff
Consult 使用 Guest Run
Guest Run 不拥有 Goal
被 Consult Agent 回复 requester
Consult 结果进入 Goal Conversation
只有当前 Goal Owner 可以 Handoff
Squad Goal 由 Squad Leader 代表 Owner
Handoff 可以回来
Handoff 必须记录完整 History
Handoff 必须终止旧 Owner Run
Handoff 必须创建新 Owner Run
Handoff Cycle 必须检测，但不直接判定 Goal Failed
Sub-goal 创建新的 Goal
Sub-goal 有独立 Owner 和 Run
Parent Goal 不因为 Sub-goal 创建而改变 Owner
Guest Run 不允许无限创建 Sub-goal
Collaboration 行为必须形成可审计 Event
Goal Owner 与 Leader 不应混为同一个概念
Goal State 与 Run State 必须保持独立
Runtime 必须通过统一 Service / Policy 强制执行以上规则

⸻

49. 一句话总结
AgentWork 的协作模型最终应该是：
                 @mention
                    |
                    v
                  Consult
                 “问一下”
                    |
                    v
                Guest Run


                  Handoff
                    |
                    v
              Ownership Transfer
                  “你接着做”
                    |
                    v
                Same Goal


                Sub-goal
                    |
                    v
                New Goal
                  “你做这个”
                    |
                    v
              Independent Run
不要再让 mention 承担任务委派或 Ownership Transfer。
这是本次协作机制优化最核心的修复原则。