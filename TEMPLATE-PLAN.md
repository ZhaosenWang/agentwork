# agentwork 小队/项目 YAML 模板创建方案

> 版本 v1（2026-09-10）· 基于当前 master（c0c6649）代码与 schema.sql 梳理
> 前端基线：/home/wangzhaosen/HuaweiWork/project/AI_Shell_WEB（agentwork 仓内 /web 已排除）

---

## 1. 背景与目标

平台当前已支持两条小队/项目的创建路径：

1. **Web 表单手工创建**：AI_Shell_WEB 的 AgentWorkSquadsView / AgentWorkProjectsView 逐个字段填写。
2. **AI SHELL 自然语言创建**（已有能力）：管家（steward）经 `agentwork create squad/domain/...` CLI → `POST /intake/dispatch` → `intakeReg` 各 handler 落库（internal/daemon/intake.go、chat_brief.go `stewardChatBriefAppendix`）。

本方案新增第三条路径：**YAML 模板创建**。在 GitCode 仓库中维护小队/项目模板，用户在 Web 上选模板、填少量覆盖值即可一键创建。要求：

- 模板存放在 GitCode 仓库，后端读取仓库内容向前端提供模板列表/详情；
- **尽量复用现有接口与服务**（agent/squad/domain/goal/schedule/skill 的 service 层，等价于走现有 HTTP CRUD）；
- 小队模板：读 YAML → 建 agent（可选 skill）→ 建 squad → 加成员；
- 项目模板：读 YAML → （可选）在 GitCode 建仓库 → 建 domain（repo/scratch）→ 建配套 agent/小队 → 建初始任务（可选定时任务）。

与「自然语言创建」的关系：两条路径**共用同一批 service 层**，互不替代；模板路径是确定性解析（不经过 LLM），结果可预期、可重复执行。P2 可让管家通过新增的 CLI 子命令代用户执行模板，实现"用 xx 模板建个小队"。

---

## 2. 现状梳理（与本方案相关的关键事实）

### 2.1 数据库（internal/store/schema.sql）

| 实体 | 表 | 创建时的关键必填/约束 |
|---|---|---|
| agent | `agent` | name 唯一、runtime_id FK、system_prompt/skills(JSON)/mcp_servers/env/max_concurrent |
| squad | `squad` | name 唯一、leader_id 必须是 agent；成员在 `squad_member`（member_type agent\|human，role 任意字符串，reviewer 有平台语义：审查卡点自动拉入） |
| 项目 domain | `domain` | type=repo（git_url 必填）\|scratch（无仓库，固定人卡点）；name 唯一；policy_text 创建即可存；checks 需编译+冻结 |
| 任务 goal | `goal` | title 必填；agent/squad 执行时 domain_id 必填；status active 时**首跑在 Create 事务内即产生**（goal.go:170） |
| 定时任务 schedule | `schedule` | name/title/cron/assignee/domain_id |
| skill | `skill` + 磁盘 | name 唯一，文件来自 zip / map，SKILL.md frontmatter 解析 name |

### 2.2 可直接复用的服务层（模板 apply 的"积木"）

- `AgentService.Create` / `UpsertByName`（agent.go:74 / agent.go:181，后者即 team-import 导入路径）
- `SquadService.Create` / `AddMember` / `UpsertByName`（squad.go:48 / 199 / 77，UpsertByName 会清空重建成员）
- `SkillService.Create(files map[string]string)` / `CreateFromZip` / `UpsertByName`（skill.go，名字从 SKILL.md 解析）
- `DomainService.Create`（domain.go:235，repo 域要求 git_url；PolicyText/验证强度可随建随存；issue 跟踪可选）
- `GoalService.Create`（goal.go:170，status=active 立即执行 / backlog 挂起）
- `ScheduleService.Create`（schedule.go:102）
- 运行时解析：`AgentService.ListActiveRuntimeIDs` / `resolveRuntime`（team_import.go:359，名字不匹配则取第一个 active）
- 名字→ID 解析模式：`resolveAgentByName` / `resolveSquadByName`（intake.go:915 / 889）
- git 连通性探测：`Daemon.TestDomainGit`（domain_git_check.go:37，ls-remote + 默认分支解析）

### 2.3 已有的"从 Git 仓库导入"先例：team-import（team_import.go）

`POST /teams/import` 走 processor-run：管家 agent 克隆仓库、探索、产出 team.json，`IngestImport` 按名字 upsert agents/skills/squad。**但它是 LLM 驱动、格式自由、异步且慢**。YAML 模板是严格契约，采用**确定性后端解析**更合适（快、免 token、幂等可控）；team-import 保留为"任意格式团队仓库"的兜底路径。注意：当前前端（AI_Shell_WEB）没有团队导入入口，两条路径互不影响。

### 2.4 前端事实（AI_Shell_WEB）

- API 层：`src/api/agentworkApi.ts`（`request()` 封装 + snake_case 契约）、`agentWorkGoalApi.ts`（goal/domain 全套）、`customAgentApi.ts`；Vite 代理 `/api/agentwork → :7373`。
- 视图：`AgentWorkSquadsView.vue`（710 行，创建小队弹窗已校验"需先有 agent 作 leader"）、`AgentWorkProjectsView.vue`（1101 行，repo/scratch 双类型表单 + 连接测试）。
- WS：`useAgentWorkWs` 按 topic 订阅；后端 `ws/hub.go` topic 白名单已含 `agent:created / squad:created / domain:created / schedule:created`——**模板 apply 落库后前端列表自动刷新，无需新事件**。

### 2.5 平台边界

- 单用户、SQLite WAL；创建类接口均为同步快速落库。
- 错误码 `AW.100000xx`（codes.go），前端 `agentWorkErrors.ts` 有 explainError 本地化机制。
- `gopkg.in/yaml.v3` 已是依赖（skill.go 用其解析 frontmatter），无新增依赖成本。

---

## 3. 整体方案

### 3.1 架构一图

```
┌─────────────────────────── GitCode 模板仓库 ───────────────────────────┐
│  registry.yaml（索引） + squads/*.yaml + projects/*.yaml + skills/…    │
└──────────────┬───────────────────────────────────────────────────────┘
               │ ① 拉取/缓存（GitCode contents API，公共仓免 token）
┌──────────────▼──────────────────── agentwork-daemon ──────────────────┐
│ TemplateService（internal/service/template.go）                        │
│   · Fetch/Refresh（拉 registry + 模板文件 → 校验 → 缓存 app_settings） │
│   · List/Get（供前端展示：元信息 + spec 预览）                         │
│ TemplateApplyService（internal/service/template_apply.go）             │
│   · ApplySquad   → AgentService / SkillService / SquadService          │
│   · ApplyProject → RepoProvider(新建仓库) / DomainService /            │
│                    AgentService / SquadService / GoalService /         │
│                    ScheduleService                                     │
│   · 每步结果记录 → 返回 per-item 摘要（部分成功可见）                  │
└──────────────┬───────────────────────────────────────────────────────┘
               │ ② 新增 HTTP 端点（handler.go Mount）
┌──────────────▼──────────────────── AI_Shell_WEB ──────────────────────┐
│ 小队页「从小队模板创建」弹窗 / 项目页「从项目模板创建」弹窗            │
│ templateApi.ts（list/get/apply/refresh）+ 类型 + 错误解释              │
└───────────────────────────────────────────────────────────────────────┘
```

### 3.2 关键设计决策

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| D1 | 解析方式 | **后端确定性 YAML 解析**，不走 processor-run/LLM | 模板是严格契约；快、零 token 消耗、可幂等重放；team-import 继续负责自由格式导入 |
| D2 | apply 是否复用 HTTP CRUD 接口 | 复用 **service 层方法**（与 HTTP handler 同一层），不绕行 intake | service 层自带全部校验与事件发布；HTTP handler 只做 JSON 解码，直接调 service 等价且能在一次请求里做多步编排 |
| D3 | 名称冲突策略 | YAML 里 `strategy: create\|upsert`（默认 `upsert`） | `upsert` 走既有 `UpsertByName`（team-import 同款语义）；`create` 遇重名报既有 AW 码（CodeAgentNameExists 等） |
| D4 | runtime 如何确定 | YAML 可写 runtime 名；缺省/不匹配 → 第一个 active runtime（复用 `resolveRuntime` 语义） | 与 team-import 完全一致，模板不需要感知机器 |
| D5 | 项目建仓库 | 新增 `internal/gitcodeapi`（REST 客户端：CreateRepo / GetContents / GetRepo），`RepoProvider` 接口留 github 扩展位 | 现有代码只有 issue API（internal/issue/gitcode.go），无建仓能力；仓库项目若新建需 `auto_init: true`（空仓会被 `TestDomainGit` 以 "repository is empty" 拒绝，handler.go:1022） |
| D6 | 模板来源配置 | `app_settings` 新键 `platform.template_repo`（git_url + branch），默认指向官方模板仓；token 可每次请求临时提供、不落库（私有仓） | 与 platform.webhook_secret 同风格；公共仓零配置可用 |
| D7 | 缓存 | 拉取结果 JSON 缓存于 `app_settings`（key `platform.template_cache`），带 fetched_at；`GET /templates` 读缓存（过期可选自动刷），`POST /templates/refresh` 强刷 | 列表毫秒级响应；GitCode 不可达时仍可用旧缓存 |
| D8 | apply 事务性 | **best-effort 顺序执行 + per-item 结果**（不追求全库事务） | service 层各自提交、SQLite 单用户；与 intake 部分成功语义一致（intake.go doCreateSquad 的 failed[]）；配合 `strategy: upsert` 可安全重放 |
| D9 | 验收策略 policy_text | 模板可携带 `policy_text` 随 domain 创建落库；**不自动编译/冻结**（编译需人确认卡点） | 编译冻结是平台"人定义验收"的守卫流程（DESIGN.md §5.3），模板不得绕过 |
| D10 | 凭据 | 建仓 token、私有模板仓 token、domain 的 git_credentials 均由**前端 apply 请求体传入**，即用即弃；domain 持久化字段照旧存 `domain.git_credentials` | 单用户平台现有约定（GitCredentials 明文入库），不新增存储面 |

### 3.3 新增接口一览（全部挂在现有 authMiddleware 下）

| 方法/路径 | 用途 | 请求 → 响应 |
|---|---|---|
| `GET /templates?type=squad\|project` | 前端模板列表（读缓存，空/过期自动尝试刷新） | → `[{id,kind,name,description,version,tags,icon,updated_at}]` |
| `GET /templates/{kind}/{id}` | 模板详情（含 spec 全文供预览/二次编辑） | → `{meta, spec_yaml, spec}` |
| `POST /templates/refresh` | 强制重拉 GitCode（可选 body.token） | → `{fetched, errors[]}` |
| `POST /templates/squads/{id}/apply` | 应用小队模板 | body 覆盖值+token → `{squad, agents[], skills[], items[]}` |
| `POST /templates/projects/{id}/apply` | 应用项目模板 | body 覆盖值+token → `{domain, repo_url?, agents[], squad?, goals[], schedules[], items[]}` |
| `PUT /settings/platform`（扩展） | `template_repo` 字段进现有平台设置 | 复用现有端点 |

### 3.4 复用矩阵（模板字段 → 现有服务调用）

| 模板 YAML 节点 | 落库动作 | 复用的现有代码 |
|---|---|---|
| `spec.skills[]`（内嵌包） | 建/更 skill | `SkillService.Create(files)`（zipx.Build 已支持 map→zip） |
| `spec.agents[]` | 建/更 agent | `AgentService.Create` / `UpsertByName`；runtime 经 `ListActiveRuntimeIDs`+名字匹配 |
| `spec.squad` + `members[]` | 建/更小队+成员 | `SquadService.Create/UpsertByName` + `AddMember`（leader 不入 members，平台既有约定） |
| `spec.repo.create` | GitCode 建仓 | 新增 `gitcodeapi.CreateRepo`（POST /repos，token 鉴权；`GET /user` 解析 owner） |
| `spec.domain` | 建项目 | `DomainService.Create`（type repo/scratch、git_url、policy_text、git_identity/credentials）；apply 前先 `Daemon.TestDomainGit` 探测（与 POST /domains handler 同门槛） |
| `spec.goals[]` | 建任务 | `GoalService.Create`（assignee 按名解析：先 team 段后平台已有；status=active 立即开跑） |
| `spec.schedules[]` | 建定时任务 | `ScheduleService.Create`（timezone 取 daemon 本地时区，同 intake） |

---

## 4. YAML 模板规范（依据现有逻辑与 schema 制定）

公共骨架（对齐 skill SKILL.md frontmatter 的 yaml 风格与 team.json 的字段命名）：

```yaml
api_version: agentwork/v1          # 固定；未来升级用
kind: squad-template | project-template
metadata:                          # 展示与索引用（registry.yaml 冗余一份便于列表页不拉正文）
  id: <kebab-case 唯一 id>
  name: <展示名>
  description: <一句话>
  version: 1.0.0
  tags: [研发, 后端]
  icon: <可选，emoji 或图标名>
spec: <见下>
```

### 4.1 小队模板（squad-template）

```yaml
api_version: agentwork/v1
kind: squad-template
metadata:
  id: backend-dev
  name: 后端开发小队
  description: 一名技术负责人带两名开发、一名审查的开发小队
  version: 1.0.0
  tags: [研发]
spec:
  # 与平台已有同名实体冲突时的策略：create=报错 | upsert=按名更新（默认 upsert）
  strategy: upsert
  agents:
    - name: backend-leader
      description: 后端技术负责人，负责拆解与委派
      system_prompt: |
        你是一名资深后端技术负责人……（完整人设，原样下发）
      skills: [feishu-notify]      # 平台已有 skill 名 或 本模板仓 skills/ 内嵌包名
      runtime: auto                # runtime 名；auto/缺省=第一个 active runtime
      max_concurrent: 3            # 缺省 3（与 AgentService.Create 默认一致）
      model: ""                    # 可选覆盖
      env: {}                      # 可选
      mcp_servers: []              # 可选，acp.McpServer 形状
    - name: backend-worker-1
      description: Go 后端开发
      system_prompt: |
        你是一名 Go 后端开发工程师……
    - name: backend-reviewer
      description: 代码审查
      system_prompt: |
        你是一名严格的代码审查员……
  squad:
    name: 后端开发小队
    description: 后端需求的拆解-开发-审查流水线
    leader: backend-leader         # 必填，引用 agents[].name
    instructions: |
      （leader 的协作规则，进入 BuildLeaderBriefing 的 Squad Instructions 段）
    members:                       # 不含 leader；role: reviewer 有平台语义（审查卡点自动拉入、禁止派活）
      - name: backend-worker-1
        role: member
      - name: backend-reviewer
        role: reviewer
```

**校验规则**（后端强校验，缺一即 400 + `AW.10000025/26/27`）：
- `spec.agents` 非空；agent.name 仓内唯一；
- `spec.squad.leader` 必须能在 agents 中解析；members.name 同理；
- leader 不允许出现在 members（平台语义：leader 存 squad.leader_id）；
- skills 引用解析顺序：模板仓内嵌包 → 平台已有 skill → 报错。

### 4.2 项目模板（project-template）

```yaml
api_version: agentwork/v1
kind: project-template
metadata:
  id: go-service
  name: Go 微服务项目
  description: 在 GitCode 新建仓库并初始化配套小队与首批任务
  version: 1.0.0
  tags: [研发, Go]
spec:
  repo:                            # ── 可选整节。省略 = 用户 apply 时自填已有 git_url ──
    create: true                   # true = 用 apply 请求携带的 token 在 GitCode 新建仓库
    visibility: private            # private | public
    auto_init: true                # 强烈建议 true：空仓无法通过 TestDomainGit（"repository is empty"）
    gitignore_template: Go         # 可选，平台侧预置 .gitignore 模板名
  domain:
    type: repo                     # repo | scratch（scratch 时 repo 节无效、git_url 留空）
    default_branch: main           # 缺省 main；新建仓以 auto_init 分支为准
    git_identity: ""               # 可选，"name <email>"
    # git_credentials 不进模板——apply 请求体传入后持久化到 domain 行（平台既有约定）
    issue_assignee: backend-dev-squad   # 可选：仓库 Issue 自动建任务的处理方（agent/squad 名）
    issue_assignee_type: squad
    policy_text: |                 # 可选：验收策略自然语言，随 domain 落库；编译+冻结仍走 Web 卡点
      所有变更需通过 go build ./... 与 go test ./...；接口变更必须同步更新文档。
  team:                            # ── 可选整节：随项目创建的 agents/squad（结构=小队模板的 agents+squad）──
    strategy: upsert
    agents:
      - name: go-owner
        description: Go 服务负责人
        system_prompt: |
          ……
      - name: go-dev-1
        description: Go 开发
        system_prompt: |
          ……
    squad:
      name: go-service 小队
      leader: go-owner
      instructions: |
        ……
      members:
        - name: go-dev-1
          role: member
  goals:                           # ── 可选：初始任务 ──
    - title: 搭建项目骨架
      description: 初始化 go module、目录结构与 CI 配置
      assignee: go-owner           # agent/squad 名：先解析 team 段，再解析平台已有
      assignee_type: agent
      start: true                  # true=active 立即执行；false=backlog（默认）
    - title: 实现 /healthz 接口
      description: 返回 200 与服务版本号
      assignee: go-service 小队
      assignee_type: squad
      start: false
  schedules: []                    # ── 可选：初始定时任务 ──
  # - name: 每日巡检
  #   title: 巡检 go-service
  #   description: 检查构建与测试
  #   cron: "0 9 * * *"
  #   assignee: go-owner
  #   assignee_type: agent
```

**字段与数据库映射表**

| YAML | 目标列/表 | 备注 |
|---|---|---|
| domain.type / name / default_branch / git_identity / git_credentials / policy_text | domain 同名列 | name 缺省用覆盖值（apply 必填 `name`）；policy_text 随建落库，checks 留待编译 |
| domain.issue_assignee(_type) | domain.issue_* | 经 `deriveIssueSource` 从 git_url 推 owner/repo+provider；需 git_credentials 同在（validateIssueTracking） |
| agents[].system_prompt / description / skills / max_concurrent | agent 同名列 | skills 存 id（解析后）；runtime→runtime_id |
| squad.name / description / instructions / leader / members | squad / squad_member | role 原样存 |
| goals[].title/description/assignee*/start | goal 同名列；start→status active/backlog | active 的首跑在 Create 事务内产生（现有行为） |
| schedules[].* | schedule 同名列 | enabled=true；timezone=daemon 本地 |

### 4.3 apply 请求体（前端覆盖值，两接口同风格）

```jsonc
// POST /templates/projects/go-service/apply
{
  "name": "订单中心",                 // 覆盖 domain.name（必填，模板 metadata.name 仅作占位默认）
  "git_url": "",                      // spec.repo 省略/ create=false 时必填；create=true 时留空由平台建仓后回填
  "git_credentials": "<token>",       // 建仓+域推拉用；repo 域必填（私有仓）或需要 push 时必填
  "template_token": "<可选>",         // 私有模板仓拉取 token（一般列表阶段已用过；apply 通常不需要）
  "repo": { "visibility": "private", "auto_init": true },   // 覆盖 spec.repo 的少量字段
  "rename": { "go-owner": "订单中心-负责人" },  // 可选：agent/squad 改名映射（避免多项目同名冲突）
  "goal_start": true                  // 可选：一键把所有 goals.start 覆盖为 true/false
}
```

`rename` 是项目模板批量复用的关键：模板里的 `go-owner` 在第二个项目 apply 时会被 rename 成新名字，天然规避 `agent.name UNIQUE` 冲突（配合 `strategy: upsert` 也安全——upsert 只在"确属同一个实体"时才应触发，改名映射让多项目互不污染）。

---

## 5. GitCode 模板仓库结构

```
agentwork-templates/                     # 独立 GitCode 仓库（公共仓即可，私有仓需 token）
├── README.md                            # 模板编写指南 + schema 说明（人读）
├── registry.yaml                        # 索引（列表页数据源，避免逐个拉模板正文）
│    templates:
│      - id: backend-dev
│        kind: squad-template
│        path: squads/backend-dev.yaml
│        name: 后端开发小队
│        description: 一名技术负责人带两名开发、一名审查
│        version: 1.0.0
│        tags: [研发]
│      - id: go-service
│        kind: project-template
│        path: projects/go-service.yaml
│        ……
├── schema/                              # 可选：JSON Schema，供编辑器校验与文档生成
│   ├── squad-template.schema.json
│   └── project-template.schema.json
├── squads/                              # 小队模板（4.1 规范）
│   ├── backend-dev.yaml
│   ├── code-review.yaml
│   └── content-ops.yaml
├── projects/                            # 项目模板（4.2 规范）
│   ├── go-service.yaml                  # 仓库项目（create: true）
│   ├── web-frontend.yaml                # 仓库项目（用户自备仓库）
│   └── weekly-report.yaml               # scratch 项目示例
└── skills/                              # 模板内嵌 skill 包（目录名=skill 名，会被 zipx 打包）
    └── feishu-notify/
        └── SKILL.md                     # frontmatter: name/description（SkillService 由此解析）
```

约定：

- `registry.yaml` 是唯一入口；registry 未列出的文件不展示（便于草稿/下线）。
- registry 与模板正文双份 metadata，Refresh 时校验一致（id 冲突即整体报错，取路径优先级 registry 声明）。
- skills/ 内嵌包按目录整目录拉取（contents API 递归或按目录列文件），文件 map 交 `SkillService.Create`。
- `kind` 与所在目录（squads/ vs projects/）做双保险校验。

---

## 6. 后端方案（agentwork）

### 6.1 新增文件

```
internal/gitcodeapi/client.go        # GitCode REST 客户端（token 走 access_token query，同 issue.GitCodeClient 风格）
│   · GetContents(ctx, ownerRepo, path, ref) (content []byte, err)   # GET /repos/{o}/{r}/contents/{path}?ref=
│   · GetDir(ctx, ownerRepo, dir, ref) ([]FileEntry, err)            # 目录列举（内嵌 skill 拉取用）
│   · CreateRepo(ctx, token, in CreateRepoInput) (fullURL, err)      # POST /repos {name, private, auto_init, gitignore_template}
│   · CurrentUser(ctx, token) (login, err)                           # GET /user（解析建仓 owner）
│   说明：端点细节以 GitCode v5 API 现行文档为准（与 issue 包同 base https://api.gitcode.com/api/v5）；
│   包内留 RepoProvider 小接口（gitcode 实现 + 未来 github 实现），CreateRepo 不直接暴露第三方形状。
internal/service/template.go         # TemplateService
│   · 拉取/校验/缓存 registry+模板；List(kind) / Get(kind,id) / Refresh(token)
│   · 缓存载体：app_settings key platform.template_cache（JSON：fetched_at + 模板全文数组）
│   · 来源配置：app_settings key platform.template_repo（JSON {git_url, branch}），SettingsService 读写，
│     默认值内置官方仓地址；PUT /settings/platform 增加 template_repo 字段（merge-write 语义不变）
internal/service/template_apply.go   # TemplateApplyService（依赖注入同 TeamImportService.SetDependencies 风格）
│   · ApplySquad(ctx, templateID, ApplyOverrides) — 调 Skill/Agent/SquadService
│   · ApplyProject(ctx, templateID, ApplyOverrides) — 建仓(可选) → TestDomainGit → DomainService.Create
│     → team 段（Skill/Agent/SquadService）→ GoalService.Create → ScheduleService.Create
│   · 返回 ApplyResult{Items: [{kind, name, id, action: created|updated|skipped|failed, error?}]}
internal/server/handler/template.go  # handler（Mount 里注册 6 条路由；风格同 squad 段）
```

### 6.2 接线（cmd/agentwork-daemon/main.go + server.go）

```go
templateSvc := service.NewTemplateService(st, settingsSvc)
templateApplySvc := service.NewTemplateApplyService(st, bus,
    agentSvc, skillSvc, squadSvc, domainSvc, goalSvc, schedSvc, d /* TestDomainGit */)
// server.New 增参（或沿用 Handlers 结构体加字段，保持现有 New 参数序列的扩展惯例）
```

`Handlers` 增加 `Templates *service.TemplateService`、`TemplateApply *service.TemplateApplyService` 两个字段（可为 nil → 500 "not configured"，同 TeamImport 的 nil 防御）。

### 6.3 apply 执行序（项目模板，逐步落库 + 结果收集）

```
1. 解析覆盖值：name 必填；git_url（create=false 时必填）；git_credentials（repo 域按需）
2. [repo.create=true] gitcodeapi.CurrentUser → CreateRepo(auto_init) → 得 git_url
3. TestDomainGit(git_url, branch, credentials)  ← 与 POST /domains 同门槛（决策 6-24 延伸）
4. DomainService.Create{type, name, git_url, default_branch, git_identity,
   git_credentials, policy_text, issue_assignee…}
5. team 段（若有）：内嵌 skill→SkillService.Create；AgentService.Create/UpsertByName
   （runtime 名→id，失配回退第一个 active）；SquadService.Create/UpsertByName + AddMember
   （rename 映射先行替换名字）
6. goals[]：assignee 按名解析（team 段实体 → 平台已有 agent/squad）→ GoalService.Create
   {status: start?active:backlog, domain_id: 新 domain, created_by_type: "human"}
7. schedules[]：同 intake 的 timezone 处理 → ScheduleService.Create
8. 汇总 Items[] 返回；任一步失败记录 failed 继续（建仓/建 domain 失败则直接终止）
```

小队模板 apply 即上表 5 的子集（skills → agents → squad → members）。

### 6.4 错误码与事件

新增 codes.go：

```go
CodeTemplateSourceUnset  = "AW.10000025" // 未配置 platform.template_repo
CodeTemplateFetchFailed  = "AW.10000026" // GitCode 拉取失败（网络/404/token）
CodeTemplateInvalid      = "AW.10000027" // schema 校验失败（detail 携带字段路径）
CodeTemplateNotFound     = "AW.10000028"
CodeRepoCreateFailed     = "AW.10000029" // 建仓失败（detail 携带 GitCode 响应）
```

事件：apply 落库复用各 service 自带事件（agent:created / squad:created / domain:created / schedule:created 均在 hub 白名单内），前端零新增订阅。`POST /templates/refresh` 可选发 `template:refreshed`（如需多端同步再加白名单，P0 不做）。

### 6.5 校验细节与边界

- **YAML 解析**：`yaml.Unmarshal` 到强类型 struct + 手写语义校验（引用完整性：leader/members/assignee/skills）；未知字段 `KnownFields(true)` 严格模式，防止拼写静默失效。
- **metadata.id 规则**：`^[a-z0-9][a-z0-9-]{1,63}$`；registry 与正文双处一致。
- **空仓防护**：repo.create 必须允许 auto_init；apply 时若目标分支在 TestDomainGit 返回 BranchExists=false 且 refs 为空 → 明确报"空仓不可作项目仓"（与 handler.go:1022 同文案）。
- **token 安全**：apply 请求里的 token 不进日志（复用 gitutil.SanitizeURL / sanitize 惯例）；模板缓存不落任何 token。
- **重放/幂等**：strategy=upsert + rename 映射 → 同一模板可对多个项目重复 apply；Items[] 里 action=updated 如实上报。
- **回滚**：不做反向删除（与 intake 部分成功一致）；Items[] 让用户清楚哪些失败，可修正后重放（upsert 语义保证已成功项不重复建）。

### 6.6 可选 P2：管家 CLI / NL 集成

- CLI（cmd/agentwork-cli）：`agentwork template list --type squad|project`、`agentwork template apply squad <id> [--name ...] [--rename a=b]`、`agentwork template apply project <id> [--name ... --git_url ...]`（输出 JSON，同既有命令风格）。
- `stewardChatBriefAppendix` 增补两个命令行说明 → 管家即可响应"用后端开发小队模板建个小队、leader 改叫 xx"。
- intake 新意图 `create_from_template`（parser 产 template id + 覆盖值）——可选，CLI 直调已够用。

---

## 7. 前端方案（AI_Shell_WEB）

### 7.1 新增 API 模块与类型

```
src/api/templateApi.ts
  listTemplates(kind)        GET /templates?type=
  getTemplate(kind, id)      GET /templates/{kind}/{id}
  refreshTemplates()         POST /templates/refresh
  applySquadTemplate(id, body)
  applyProjectTemplate(id, body)
  （复用 agentworkApi.ts 的 request / AgentworkApiError；错误经 explainError 扩展）
src/types/templateApiTypes.ts
  TemplateSummary / TemplateDetail / ApplySquadResult / ApplyProjectResult / ApplyItem
src/api/agentWorkErrors.ts   增加 AW.10000025~29 的中文文案映射
```

### 7.2 小队页（AgentWorkSquadsView.vue）

- 头部「创建小队」旁加「从小队模板创建」（图标 FileBox/LayoutTemplate）。
- 弹窗流程（新建组件 `src/components/agentWork/SquadTemplateDialog.vue`）：
  1. **选模板**：卡片列表（name/description/tags/version；右上角刷新图标调 refresh）；
  2. **预览**：选中后展示模板摘要——将创建哪些 agent（人设前 2 行）、leader、成员与角色、引用 skills；展开可看原始 YAML（getTemplate 的 spec_yaml，只读）；
  3. **覆盖**：可编辑 agent 名称（默认模板名；两两查重 + 与平台已有 agent 查重）、小队名称；若平台无 active agent runtime 则沿用现有 PrerequisiteDialog 提示；
  4. **提交**：applySquadTemplate → 成功 toast 逐项结果（"已创建 agent ×3、小队 ×1"）；失败项列表展示 error；关闭后依赖既有 WS（agent:created / squad:created）自动刷新列表，无需手动 load。

### 7.3 项目页（AgentWorkProjectsView.vue）

- 「创建项目」旁加「从项目模板创建」（新建 `ProjectTemplateDialog.vue`）：
  1. **选模板**：同上（kind=project）；scratch 类模板自动隐藏仓库相关字段；
  2. **预览**：将创建的仓库（新建/自备）、domain 配置、配套小队成员、初始任务清单（含是否立即执行）；
  3. **覆盖表单**（按模板能力动态显隐，字段命名对齐现有项目表单习惯）：
     - 项目名称（必填）
     - 仓库来源：`新建仓库`（visibility/auto_init 开关）| `使用已有仓库`（repoUrl + repoToken + defaultBranch，复用现有 testDomainGit 连接测试交互）
     - GitCode token（create=true 或私有仓时必填；说明文案：仅本次建仓与仓库读写使用）
     - agent 改名（rename 键值编辑器，默认全部保留原名）
     - 初始任务是否立即开始（goal_start 开关）
  4. **提交**：applyProjectTemplate → 提交期间 loading（建仓+探测约数秒）；成功后 toast 汇总（"已建仓库、项目、小队 ×1、任务 ×2"），关闭弹窗并选中新建项目（返回体 domain.id → store 高亮）。

### 7.4 模板源配置（SettingsView.vue，最小改动）

- 平台设置区新增「模板仓库」：git_url + branch（对应 `PUT /settings/platform` 新字段 template_repo），留空即用内置默认仓。
- 不做模板 CRUD 页面（模板维护在 GitCode 仓库侧完成，平台只读——这是本方案的立意）。

### 7.5 交互守则（对齐项目现有模式）

- 所有错误经 `explainError`（coded error → detail 插值），apply 的 per-item 失败用结果列表呈现而非整体 toast 一条。
- 弹窗内不缓存 token；关闭即丢弃。
- 列表刷新完全依赖既有 WS topic，新增零订阅。

---

## 8. 分期实施计划

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P0 小队模板闭环** | template.go（拉取/缓存/List/Get/Refresh）+ ApplySquad + 4 个 HTTP 端点 + 错误码；GitCode 模板仓初始化（registry + 1~2 个 squad 模板）；前端 SquadTemplateDialog + templateApi | 从模板一键建出 agent×N + 小队；重放 upsert 安全；列表毫秒级（缓存） |
| **P1 项目模板闭环** | gitcodeapi（CreateRepo/GetContents/CurrentUser）+ ApplyProject（建仓→探测→domain→team→goals→schedules）+ 2 端点；前端 ProjectTemplateDialog；settings 加 template_repo 字段 | 从模板一键建出"仓库+项目+小队+任务"，任务 active 即开跑；scratch 模板同流程可跑 |
| **P2 增强** | 管家 CLI `template list/apply` + steward brief 增补（NL 走模板）；模板内嵌 skill 拉取（若 P0 未含）；`template:refreshed` WS + 多端同步；JSON Schema 发布 + 文档 | "用 xx 模板建个小队/项目"在 AI SHELL 对话中可用 |

P0 预估：后端 ~700 行 + 前端 ~500 行 + 模板仓若干文件；P1 再各加 ~400 行。

---

## 9. 风险与待确认问题

| # | 风险/问题 | 处理 |
|---|---|---|
| R1 | GitCode 建仓 API（POST /repos）参数形状需以现行文档核实（auto_init/gitignore_template 是否支持） | gitcodeapi 包内聚；联调阶段用真实 token 打通，必要时降级"先建空仓→平台推首个 commit"（gitCloneURL 已有 push 链路） |
| R2 | 模板仓为私有仓时的 token 管理 | 仅请求级传入，不落库；settings 可存一个"模板仓只读 token"（用户显式配置才存） |
| R3 | agent 重名导致跨项目串改（upsert 误伤） | rename 映射 + 前端默认提示改名；或团队对"复用同名 agent"本就有预期（team-import 同语义） |
| R4 | goals[].start=true 的 active 任务在 apply 后立刻消耗机器执行 | 前端默认 goal_start=false，由用户显式开启 |
| R5 | 模板与平台版本漂移（字段增删） | api_version + KnownFields 严格校验：不认识的字段直接报错，宁可失败不可静默丢配置 |
| Q1 | 模板仓地址：是否使用独立 GitCode 仓库（推荐）还是放在某个现有仓库子目录？ | 待定，settings 可改，不阻塞开发 |
| Q2 | 项目模板是否需要支持 `schedules` 之外的"项目级内置说明文档"（如自动写入 AGENTWORK.md）？ | 建议不做：AGENTWORK.md 属平台注入命名空间（domain.go Checks.Excludes 注释） |
| Q3 | apply 是否需要进入"预览→确认"两步（前端先 dry-run）？ | P0 单步+结果摘要即可；dry-run 可作 P2 增强 |
