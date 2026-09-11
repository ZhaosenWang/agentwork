# agentwork-templates

agentwork 平台的官方模板仓库：维护**小队模板**（squad-template）与**项目模板**（project-template），
供 Web 端「从模板创建」一键落地（创建 agent/小队/项目仓库/初始任务）。

## 仓库结构

```
agentwork-templates/
├── registry.yaml          # 唯一索引：平台只展示这里列出的模板
├── squads/                # 小队模板（squad-template）
│   ├── backend-dev.yaml
│   └── code-review.yaml
├── projects/              # 项目模板（project-template）
│   ├── go-service.yaml    # GitCode 新建仓库型
│   ├── existing-repo.yaml # 用户自备仓库型
│   └── weekly-report.yaml # scratch 无仓库型
└── skills/                # 模板内嵌 skill 包（目录名 = skill 名）
    └── daily-report/
        └── SKILL.md
```

## 编写规则

1. **registry.yaml 必须登记**：`id`（kebab-case，作 URL 路径）、`kind`、`path`；
   正文里的 `metadata.id/kind` 必须与 registry 一致，否则该模板被跳过并记录错误。
2. **api_version 固定 `agentwork/v1`**；未知字段会被拒绝（严格解析，拼写错误会报错而不是静默忽略）。
3. **小队模板**：`spec.agents[]` 至少 1 个；`spec.squad.leader` 必须能解析到 agents 中的名字；
   leader 不写进 `members`；`members[].role: reviewer` 有平台语义（审批时自动拉入审查）。
4. **项目模板**：`repo` 节可选——`create: true` 表示 apply 时用用户提供的 token 在 GitCode 新建仓库
   （`auto_init` 默认 true，空仓无法通过平台的 git 连接测试）；省略 repo 节则 apply 时必须提供已有
   `git_url`。`domain.type: scratch` 表示无仓库项目。
5. **skill 引用**：先在模板仓 `skills/<名字>/` 找内嵌包，再找平台已有 skill，都找不到则该 agent 创建失败。
6. 模板维护即改这个仓库、提交推送；平台侧点「刷新」生效（`platform.template_repo` 可指向本仓的 fork）。

## 平台侧使用

- Web 小队页 → 「从模板创建」：列表来自 `GET /templates?type=squad`，一键 `POST /templates/squads/{id}/apply`
- Web 项目页 → 「从模板创建」：`GET /templates?type=project`，`POST /templates/projects/{id}/apply`
- 拉取实现为 shallow clone，缓存于 daemon 本地（`platform.template_cache`）；列表页在缓存过期时自动重拉
