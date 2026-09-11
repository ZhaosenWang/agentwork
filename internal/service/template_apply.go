package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/gitcodeapi"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
	"gopkg.in/yaml.v3"
)

// templateSpec is the shared envelope of both template kinds: api_version +
// metadata + spec. The spec is decoded per kind (squadSpec / projectSpec)
// with KnownFields(true) — a misspelled field must fail loudly, not silently
// drop its config (R5 in TEMPLATE-PLAN.md).
type templateSpec struct {
	APIVersion string `yaml:"api_version"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		ID          string   `yaml:"id"`
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Version     string   `yaml:"version"`
		Tags        []string `yaml:"tags"`
		Icon        string   `yaml:"icon"`
	} `yaml:"metadata"`
	Spec yaml.Node `yaml:"spec"`
}

// ConflictStrategy picks the name-collision behavior: create fails loudly on
// an existing name (the existing AW codes surface); upsert updates by name
// (the team-import semantics). Default upsert.
const (
	conflictCreate = "create"
	conflictUpsert = "upsert"
)

// ── squad-template spec ──

type squadSpec struct {
	Strategy string     `yaml:"strategy"` // create | upsert (default upsert)
	Agents   []tplAgent `yaml:"agents"`
	Squad    tplSquad   `yaml:"squad"`
}

// tplAgent is one agent to create/update. Runtime names the runtime
// (mismatched/absent → first active runtime — team_import.resolveRuntime
// semantics); MaxConcurrent defaults to the service's own default.
type tplAgent struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	SystemPrompt  string            `yaml:"system_prompt"`
	Skills        []string          `yaml:"skills"`
	Runtime       string            `yaml:"runtime"`
	MaxConcurrent int               `yaml:"max_concurrent"`
	Model         string            `yaml:"model"`
	Env           map[string]string `yaml:"env"`
	McpServers    []map[string]any  `yaml:"mcp_servers"`
}

type tplSquad struct {
	Name         string      `yaml:"name"`
	Description  string      `yaml:"description"`
	Leader       string      `yaml:"leader"`
	Instructions string      `yaml:"instructions"`
	Members      []tplMember `yaml:"members"`
}

type tplMember struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"`
}

// ── project-template spec ──

type projectSpec struct {
	Repo      *tplRepoCreate `yaml:"repo"` // nil = user supplies git_url
	Domain    tplDomain      `yaml:"domain"`
	Team      *squadSpec     `yaml:"team"` // optional agents+squad riding the project
	Goals     []tplGoal      `yaml:"goals"`
	Schedules []tplSchedule  `yaml:"schedules"`
}

type tplRepoCreate struct {
	Create            bool   `yaml:"create"`
	Visibility        string `yaml:"visibility"` // private | public
	AutoInit          *bool  `yaml:"auto_init"`  // nil = true (an empty repo cannot pass the git probe)
	GitignoreTemplate string `yaml:"gitignore_template"`
	Org               string `yaml:"org"` // '' = token's user
	// Name/Description are NOT template fields — apply stamps them from the
	// (required) domain name. They exist on the struct so applyRepo can carry
	// the overlay input as one value.
	Name        string `yaml:"-"`
	Description string `yaml:"-"`
}

type tplDomain struct {
	Type              string `yaml:"type"` // repo | scratch (default repo)
	DefaultBranch     string `yaml:"default_branch"`
	GitIdentity       string `yaml:"git_identity"`
	IssueAssignee     string `yaml:"issue_assignee"`
	IssueAssigneeType string `yaml:"issue_assignee_type"`
	PolicyText        string `yaml:"policy_text"`
}

type tplGoal struct {
	Title        string `yaml:"title"`
	Description  string `yaml:"description"`
	Assignee     string `yaml:"assignee"`      // agent or squad name
	AssigneeType string `yaml:"assignee_type"` // agent | squad (default agent)
	Start        bool   `yaml:"start"`         // true = active (executes immediately); false = backlog
}

type tplSchedule struct {
	Name         string `yaml:"name"`
	Title        string `yaml:"title"`
	Description  string `yaml:"description"`
	Cron         string `yaml:"cron"`
	Assignee     string `yaml:"assignee"`
	AssigneeType string `yaml:"assignee_type"` // agent | squad (default agent)
}

// ── apply requests / results ──

// ApplySquadOverrides are the user's per-apply inputs (the dialog's fields).
type ApplySquadOverrides struct {
	// SquadName overrides spec.squad.name ('' = template value).
	SquadName string `json:"squad_name,omitempty"`
	// Rename maps template agent names to per-apply names ({"backend-leader":
	// "订单-负责人"}). Unlisted names keep their template value.
	Rename map[string]string `json:"rename,omitempty"`
	// Strategy overrides spec.strategy.
	Strategy string `json:"strategy,omitempty"`
}

// ApplyProjectOverrides are the project dialog's fields.
type ApplyProjectOverrides struct {
	Name           string            `json:"name"`              // required: the domain name
	GitURL         string            `json:"git_url,omitempty"` // required unless spec.repo.create
	GitCredentials string            `json:"git_credentials,omitempty"`
	RepoToken      string            `json:"repo_token,omitempty"`     // the repo-creation token (gitcode)
	TemplateToken  string            `json:"template_token,omitempty"` // unused by apply (list phase); kept for symmetry
	Repo           *tplRepoCreate    `json:"repo,omitempty"`           // overlay on spec.repo
	Rename         map[string]string `json:"rename,omitempty"`
	Strategy       string            `json:"strategy,omitempty"`
	GoalStart      *bool             `json:"goal_start,omitempty"` // overlay on every goal's start
}

// ApplyItem is one entity's per-step outcome.
type ApplyItem struct {
	Kind   string `json:"kind"` // skill | agent | squad | domain | repo | goal | schedule
	Name   string `json:"name"`
	ID     string `json:"id,omitempty"`
	Action string `json:"action"` // created | updated | skipped | failed
	Error  string `json:"error,omitempty"`
}

// ApplyResult is the apply response: the created roots + every step.
type ApplySquadResult struct {
	Squad  *Squad      `json:"squad,omitempty"`
	Agents []*Agent    `json:"agents,omitempty"`
	Skills []*Skill    `json:"skills,omitempty"`
	Items  []ApplyItem `json:"items"`
}

type ApplyProjectResult struct {
	Domain    *Domain     `json:"domain,omitempty"`
	RepoURL   string      `json:"repo_url,omitempty"` // set when the repo was created
	Agents    []*Agent    `json:"agents,omitempty"`
	Squad     *Squad      `json:"squad,omitempty"`
	Goals     []*Goal     `json:"goals,omitempty"`
	Schedules []*Schedule `json:"schedules,omitempty"`
	Items     []ApplyItem `json:"items"`
}

// TemplateApplyService orchestrates a template apply through the existing
// services (the same layer the HTTP CRUD handlers call — no shortcuts).
// Failure policy: best-effort sequence with per-item results; the domain/repo
// step (the apply's root) is terminal — failing it aborts the rest.
type TemplateApplyService struct {
	st        *store.Store
	templates *TemplateService
	agentSvc  *AgentService
	skillSvc  *SkillService
	squadSvc  *SquadService
	domainSvc *DomainService
	goalSvc   *GoalService
	schedSvc  *ScheduleService
	gitTester GitTester
	repoProv  gitcodeapi.RepoProvider
}

// GitTester abstracts the daemon's TestDomainGit probe (the create-domain
// gate) so the service stays daemon-free in tests.
type GitTester interface {
	TestDomainGit(ctx context.Context, gitURL, defaultBranch, credentials string) *DomainGitProbeResult
}

// DomainGitProbeResult mirrors daemon.DomainGitTestResult (the daemon package
// imports service, so the interface uses this service-side copy — one field
// mapping at the wiring site).
type DomainGitProbeResult struct {
	OK             bool
	BranchExists   bool
	ResolvedBranch string
	Refs           []string
	Error          string
}

// NewTemplateApplyService wires the orchestration (nil repoProv = repo
// creation unavailable → ApplyProject with spec.repo.create fails cleanly).
func NewTemplateApplyService(
	st *store.Store,
	templates *TemplateService,
	agentSvc *AgentService,
	skillSvc *SkillService,
	squadSvc *SquadService,
	domainSvc *DomainService,
	goalSvc *GoalService,
	schedSvc *ScheduleService,
	gitTester GitTester,
) *TemplateApplyService {
	return &TemplateApplyService{
		st: st, templates: templates, agentSvc: agentSvc, skillSvc: skillSvc,
		squadSvc: squadSvc, domainSvc: domainSvc, goalSvc: goalSvc,
		schedSvc: schedSvc, gitTester: gitTester,
		repoProv: gitcodeapi.New(),
	}
}

// SetGitTester wires the daemon's git probe after construction (the daemon
// is created after the services in main.go — same late-wiring pattern as
// SetDependencies). nil = the probe is skipped (tests).
func (s *TemplateApplyService) SetGitTester(t GitTester) { s.gitTester = t }

// ── parsing helpers ──

// parseTemplate decodes and validates a template YAML document into its meta
// + decoded spec (strict fields per kind).
func parseTemplate(raw string) (TemplateMeta, any, error) {
	var spec templateSpec
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return TemplateMeta{}, nil, fmt.Errorf("模板格式错误：%v", err)
	}
	if spec.APIVersion != "" && spec.APIVersion != "agentwork/v1" {
		return TemplateMeta{}, nil, fmt.Errorf("不支持的 api_version %q", spec.APIVersion)
	}
	if spec.Metadata.ID == "" {
		return TemplateMeta{}, nil, fmt.Errorf("模板缺少 metadata.id")
	}
	// Decode spec per kind with strict fields. yaml.Node.Decode does not
	// inherit the document decoder's KnownFields, so the spec node is
	// re-encoded through a strict decoder explicitly.
	var out any
	strictSpec := func(node yaml.Node, v any) error {
		buf, err := yaml.Marshal(&node)
		if err != nil {
			return err
		}
		d := yaml.NewDecoder(bytes.NewReader(buf))
		d.KnownFields(true)
		return d.Decode(v)
	}
	switch spec.Kind {
	case TemplateKindSquad:
		sq := &squadSpec{}
		if err := strictSpec(spec.Spec, sq); err != nil {
			return TemplateMeta{}, nil, fmt.Errorf("spec 校验失败：%v", err)
		}
		out = sq
	case TemplateKindProject:
		pr := &projectSpec{}
		if err := strictSpec(spec.Spec, pr); err != nil {
			return TemplateMeta{}, nil, fmt.Errorf("spec 校验失败：%v", err)
		}
		out = pr
	default:
		return TemplateMeta{}, nil, fmt.Errorf("模板 kind %q 不受支持", spec.Kind)
	}
	meta := TemplateMeta{
		APIVersion: spec.APIVersion, Kind: spec.Kind,
		ID: spec.Metadata.ID, Name: spec.Metadata.Name,
		Description: spec.Metadata.Description, Version: spec.Metadata.Version,
	}
	return meta, out, nil
}

// applyStrategy resolves the effective conflict strategy.
func applyStrategy(specStrategy, override string) string {
	if override == conflictCreate || override == conflictUpsert {
		return override
	}
	if specStrategy == conflictCreate {
		return conflictCreate
	}
	return conflictUpsert
}

// rename applies the rename map: the mapped name wins; unmapped keep theirs.
func rename(m map[string]string, name string) string {
	if n, ok := m[name]; ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	return name
}

// resolveSkillID maps a template skill reference to a platform skill id.
// Order: an embedded package from the template snapshot (upserted by name) →
// an existing platform skill by name. ok=false when neither resolves.
func (s *TemplateApplyService) resolveSkillID(ctx context.Context, name string) (string, bool, error) {
	if pkg, ok, err := s.templates.SkillPackage(ctx, name); err != nil {
		return "", false, err
	} else if ok {
		sk, err := s.skillSvc.UpsertByName(ctx, name, "", pkg)
		if err != nil {
			return "", false, fmt.Errorf("导入模板内嵌 skill %q：%v", name, err)
		}
		return sk.ID, true, nil
	}
	var id string
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM skill WHERE name=?`, name).Scan(&id)
	if err == nil && id != "" {
		return id, true, nil
	}
	return "", false, nil
}

// resolveAgentSkillIDs resolves every skill reference for one agent; unknown
// names fail the agent (a silent drop would ship an agent missing its tools).
func (s *TemplateApplyService) resolveAgentSkillIDs(ctx context.Context, skills []string) ([]string, error) {
	var ids []string
	for _, name := range skills {
		id, ok, err := s.resolveSkillID(ctx, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("skill %q 不存在（平台与模板包中都没有）", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// activeRuntimeIDs lists active runtime ids (the agent-creation fallback).
func (s *TemplateApplyService) activeRuntimeIDs(ctx context.Context) ([]string, error) {
	return s.agentSvc.ListActiveRuntimeIDs(ctx)
}

// resolveRuntime maps a template runtime name to an id; unmatched/empty falls
// back to the first active runtime (team-import semantics). Requires at
// least one active runtime — there is nowhere to run otherwise.
func (s *TemplateApplyService) resolveRuntime(ctx context.Context, name string, activeIDs []string) (string, error) {
	if name != "" {
		var id string
		if err := s.st.DB().QueryRowContext(ctx,
			`SELECT id FROM runtime WHERE name=? AND status='active'`, name).Scan(&id); err == nil && id != "" {
			return id, nil
		}
	}
	if len(activeIDs) == 0 {
		return "", NewValidationError("没有可用的 active runtime——先连接一台机器（agentwork connect）")
	}
	return activeIDs[0], nil
}

// agentIDByName resolves a platform agent id by exact name (” = not found).
func (s *TemplateApplyService) agentIDByName(ctx context.Context, name string) string {
	var id string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT id FROM agent WHERE name=?`, name).Scan(&id)
	return id
}

// squadByName resolves a platform squad by exact name (nil = not found).
func (s *TemplateApplyService) squadByName(ctx context.Context, name string) (*Squad, error) {
	all, err := s.squadSvc.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

// upsertAgent creates or updates one agent per strategy; returns the agent
// and the action label.
func (s *TemplateApplyService) upsertAgent(ctx context.Context, strategy string, a tplAgent, finalName string, activeIDs []string) (*Agent, string, error) {
	skillIDs, err := s.resolveAgentSkillIDs(ctx, a.Skills)
	if err != nil {
		return nil, "", err
	}
	runtimeID, err := s.resolveRuntime(ctx, a.Runtime, activeIDs)
	if err != nil {
		return nil, "", err
	}
	if strategy == conflictCreate {
		out, err := s.agentSvc.Create(ctx, Agent{
			Name: finalName, Description: a.Description, RuntimeID: runtimeID,
			SystemPrompt: a.SystemPrompt, Model: a.Model, Env: a.Env,
			Skills: skillIDs, MaxConcurrent: a.MaxConcurrent,
		})
		if err != nil {
			return nil, "", err
		}
		return out, "created", nil
	}
	// Upsert: check existence BEFORE the call — UpsertByName does not
	// distinguish created from updated.
	action := "created"
	if existing := s.agentIDByName(ctx, finalName); existing != "" {
		action = "updated"
	}
	out, err := s.agentSvc.UpsertByName(ctx, finalName, a.Description, a.SystemPrompt, runtimeID, skillIDs)
	if err != nil {
		return nil, "", err
	}
	return out, action, nil
}

// applySquadSpec is the shared squad/team section (used by both apply kinds).
// teamFinalName overrides the squad name (” = template value with rename).
func (s *TemplateApplyService) applySquadSpec(ctx context.Context, spec *squadSpec, strategy string, renameMap map[string]string, teamFinalName string) (*ApplySquadResult, error) {
	if len(spec.Agents) == 0 {
		return nil, NewValidationError("模板 spec.agents 不能为空")
	}
	res := &ApplySquadResult{}
	activeIDs, err := s.activeRuntimeIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出 active runtime：%v", err)
	}
	// Skill packages ride first so agent skill resolution sees fresh ids.
	agentIDs := map[string]string{}
	agentNameSet := map[string]bool{}
	for _, a := range spec.Agents {
		finalName := rename(renameMap, a.Name)
		if agentNameSet[finalName] {
			res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, Action: "failed", Error: "模板内 agent 名重复"})
			continue
		}
		agentNameSet[finalName] = true
		out, action, err := s.upsertAgent(ctx, strategy, a, finalName, activeIDs)
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, Action: "failed", Error: err.Error()})
			continue
		}
		agentIDs[a.Name] = out.ID // roster references use TEMPLATE names
		res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, ID: out.ID, Action: action})
		res.Agents = append(res.Agents, out)
	}
	if len(agentIDs) == 0 {
		return res, NewValidationError("模板中没有任何 agent 创建成功，小队不创建")
	}
	// Squad: the leader must resolve among the CREATED agents. The squad is
	// the apply's root — a missing leader aborts the rest (the per-agent
	// items above remain reported).
	leaderID, ok := agentIDs[spec.Squad.Leader]
	if !ok {
		res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: rename(renameMap, spec.Squad.Name), Action: "failed", Error: fmt.Sprintf("squad.leader %q 未在模板 agents 中定义或创建失败", spec.Squad.Leader)})
		return res, NewValidationError(fmt.Sprintf("squad.leader %q 未在模板 agents 中定义或创建失败", spec.Squad.Leader))
	}
	squadName := teamFinalName
	if squadName == "" {
		squadName = rename(renameMap, spec.Squad.Name)
	}
	var members []SquadMember
	for _, m := range spec.Squad.Members {
		if m.Name == spec.Squad.Leader {
			continue // leader is squad.leader_id, never a member row (platform invariant)
		}
		mid, ok := agentIDs[m.Name]
		if !ok {
			res.Items = append(res.Items, ApplyItem{Kind: "member", Name: rename(renameMap, m.Name), Action: "failed", Error: "agent 未创建成功，成员未加入"})
			continue
		}
		role := m.Role
		if role == "" {
			role = "member"
		}
		members = append(members, SquadMember{MemberType: "agent", MemberID: mid, Role: role})
	}
	var sq *Squad
	if strategy == conflictCreate {
		sq, err = s.squadSvc.Create(ctx, Squad{
			Name: squadName, Description: spec.Squad.Description,
			LeaderID: leaderID, Instructions: spec.Squad.Instructions,
		})
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: squadName, Action: "failed", Error: err.Error()})
			return res, err
		}
	} else {
		sq, err = s.squadSvc.UpsertByName(ctx, squadName, spec.Squad.Description, leaderID, spec.Squad.Instructions, members)
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: squadName, Action: "failed", Error: err.Error()})
			return res, err
		}
	}
	res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: squadName, ID: sq.ID, Action: "created"})
	if strategy == conflictCreate {
		for _, m := range members {
			if _, err := s.squadSvc.AddMember(ctx, sq.ID, m.MemberType, m.MemberID, m.Role); err != nil {
				res.Items = append(res.Items, ApplyItem{Kind: "member", Name: m.MemberID, Action: "failed", Error: err.Error()})
				continue
			}
			res.Items = append(res.Items, ApplyItem{Kind: "member", Name: m.MemberID, Action: "created"})
		}
	}
	res.Squad = sq
	return res, nil
}

// ApplySquad applies a squad template: agents (+embedded skills) → squad →
// members. Best-effort: per-agent failures are reported and skipped; the
// squad is created when its leader (and the roster) resolved.
func (s *TemplateApplyService) ApplySquad(ctx context.Context, templateID string, ov ApplySquadOverrides) (*ApplySquadResult, error) {
	raw, err := s.templates.LoadRaw(ctx, TemplateKindSquad, templateID)
	if err != nil {
		return nil, err
	}
	_, specAny, err := parseTemplate(raw)
	if err != nil {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, err.Error(), map[string]any{"id": templateID})
	}
	spec, ok := specAny.(*squadSpec)
	if !ok {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, "模板不是 squad-template", map[string]any{"id": templateID})
	}
	strategy := applyStrategy(spec.Strategy, ov.Strategy)
	return s.applySquadSpec(ctx, spec, strategy, ov.Rename, ov.SquadName)
}

// applyRepo creates the project repo when spec.repo.create (the token comes
// from the apply request; auto_init defaults true — an empty repo cannot
// pass the domain git probe). Returns the clone URL.
func (s *TemplateApplyService) applyRepo(ctx context.Context, spec *projectSpec, ov *ApplyProjectOverrides, items *[]ApplyItem) (string, error) {
	if spec.Repo == nil || !spec.Repo.Create {
		return "", nil
	}
	if s.repoProv == nil {
		return "", NewCodedError(CodeRepoCreateFailed, "仓库创建服务不可用")
	}
	token := ov.RepoToken
	if token == "" {
		return "", NewValidationError("该模板需要在 GitCode 新建仓库——请提供 repo_token")
	}
	in := *spec.Repo
	if ov.Repo != nil { // the request overlay wins field-by-field
		ov.Repo.Create = true
		if ov.Repo.Visibility != "" {
			in.Visibility = ov.Repo.Visibility
		}
		if ov.Repo.AutoInit != nil {
			in.AutoInit = ov.Repo.AutoInit
		}
		if ov.Repo.GitignoreTemplate != "" {
			in.GitignoreTemplate = ov.Repo.GitignoreTemplate
		}
	}
	if in.AutoInit == nil {
		in.AutoInit = boolPtr(true)
	}
	in.Name = ov.Name // the repo name follows the (required) domain name
	in.Description = ""
	cloneURL, err := s.repoProv.CreateRepo(ctx, token, gitcodeapi.CreateRepoInput{
		Org: in.Org, Name: in.Name, Description: in.Description,
		Private: in.Visibility != "public", AutoInit: *in.AutoInit,
		GitignoreTemplate: in.GitignoreTemplate,
	})
	if err != nil {
		*items = append(*items, ApplyItem{Kind: "repo", Name: in.Name, Action: "failed", Error: err.Error()})
		return "", NewCodedErrorDetail(CodeRepoCreateFailed, fmt.Sprintf("GitCode 建仓失败：%v", err),
			map[string]any{"name": in.Name})
	}
	*items = append(*items, ApplyItem{Kind: "repo", Name: in.Name, Action: "created"})
	return cloneURL, nil
}

// probeGit runs the same config-time git gate the POST /domains handler
// applies (决策 6-24 延伸) — misconfiguration surfaces here, not at first run.
func (s *TemplateApplyService) probeGit(ctx context.Context, gitURL, branch, credentials string) error {
	if s.gitTester == nil || gitURL == "" {
		return nil
	}
	res := s.gitTester.TestDomainGit(ctx, gitURL, branch, credentials)
	if !res.OK {
		return NewValidationError("仓库连接测试失败：" + res.Error)
	}
	if !res.BranchExists {
		if len(res.Refs) == 0 {
			return NewValidationError("仓库为空（没有任何分支）——不能用作物项目仓；建仓请开启 auto_init 或先推送一个提交")
		}
		return NewValidationError(fmt.Sprintf("分支 %q 不存在（远端分支：%s）", res.ResolvedBranch, strings.Join(res.Refs, ", ")))
	}
	return nil
}

// ApplyProject applies a project template:
// repo(optional) → probe → domain → team(agents+squad) → goals → schedules.
// The repo/domain step is terminal (everything hangs off the domain);
// goal/schedule failures are reported per-item and skipped.
func (s *TemplateApplyService) ApplyProject(ctx context.Context, templateID string, ov ApplyProjectOverrides) (*ApplyProjectResult, error) {
	raw, err := s.templates.LoadRaw(ctx, TemplateKindProject, templateID)
	if err != nil {
		return nil, err
	}
	_, specAny, err := parseTemplate(raw)
	if err != nil {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, err.Error(), map[string]any{"id": templateID})
	}
	spec, ok := specAny.(*projectSpec)
	if !ok {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, "模板不是 project-template", map[string]any{"id": templateID})
	}
	res := &ApplyProjectResult{}
	if strings.TrimSpace(ov.Name) == "" {
		return nil, NewFieldRequiredError("name")
	}
	// 1. Repo (optional).
	gitURL := strings.TrimSpace(ov.GitURL)
	if created, err := s.applyRepo(ctx, spec, &ov, &res.Items); err != nil {
		return res, err
	} else if created != "" {
		gitURL = created
		res.RepoURL = created
	}
	// 2. Domain config.
	dType := spec.Domain.Type
	if dType == "" {
		dType = "repo"
	}
	if dType != "repo" && dType != "scratch" {
		return res, NewValidationError("domain.type 必须是 repo 或 scratch")
	}
	if dType == "repo" && gitURL == "" {
		return res, NewValidationError("repo 类型项目需要 git_url（或模板声明 repo.create 并提供 repo_token）")
	}
	branch := spec.Domain.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	// 3. Probe (repo domains only — the create-domain gate).
	if dType == "repo" {
		if err := s.probeGit(ctx, gitURL, branch, ov.GitCredentials); err != nil {
			return res, err
		}
	}
	// 4. Domain. Domain names are UNIQUE by design (no upsert variant
	// exists) — the existing AW.10000010 surfaces on a repeat apply.
	domain := Domain{
		Type: dType, Name: ov.Name, GitURL: gitURL,
		DefaultBranch: branch, GitIdentity: spec.Domain.GitIdentity,
		GitCredentials: ov.GitCredentials, PolicyText: spec.Domain.PolicyText,
		IssueAssignee: spec.Domain.IssueAssignee, IssueAssigneeType: spec.Domain.IssueAssigneeType,
	}
	// Issue-assignee names resolve AFTER the team section — defer by storing
	// the raw name and resolving post-team (the service validates ids).
	createdDomain, err := s.domainSvc.Create(ctx, domain)
	if err != nil {
		res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: ov.Name, Action: "failed", Error: err.Error()})
		return res, err
	}
	res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: createdDomain.Name, ID: createdDomain.ID, Action: "created"})
	res.Domain = createdDomain
	// 5. Team (optional): reuse the squad path.
	if spec.Team != nil {
		teamStrategy := applyStrategy(spec.Team.Strategy, ov.Strategy)
		sqRes, err := s.applySquadSpec(ctx, spec.Team, teamStrategy, ov.Rename, "")
		if err != nil {
			res.Items = append(res.Items, sqRes.Items...) // per-agent outcomes already recorded
			return res, err
		}
		res.Agents = sqRes.Agents
		res.Squad = sqRes.Squad
		res.Items = append(res.Items, sqRes.Items...)
	}
	// Issue assignee: resolve the template's agent/squad NAME to an id now
	// (after the team section) and patch the domain. Skipped when unresolved
	// (validateIssueTracking would reject a phantom id).
	if dType == "repo" && spec.Domain.IssueAssignee != "" {
		if id := s.resolveIssueAssignee(ctx, spec.Domain.IssueAssignee, spec.Domain.IssueAssigneeType); id != "" {
			updated, err := s.domainSvc.Update(ctx, createdDomain.ID, Domain{
				GitURL: createdDomain.GitURL, DefaultBranch: createdDomain.DefaultBranch,
				GitIdentity: createdDomain.GitIdentity, GitCredentials: ov.GitCredentials,
				IssueRepo: createdDomain.IssueRepo, IssueAssignee: id,
				IssueAssigneeType: spec.Domain.IssueAssigneeType,
				IssueProvider:     createdDomain.IssueProvider,
			})
			if err != nil {
				res.Items = append(res.Items, ApplyItem{Kind: "domain.issue_assignee", Name: spec.Domain.IssueAssignee, Action: "failed", Error: err.Error()})
			} else {
				res.Domain = updated
				res.Items = append(res.Items, ApplyItem{Kind: "domain.issue_assignee", Name: spec.Domain.IssueAssignee, ID: id, Action: "updated"})
			}
		} else {
			res.Items = append(res.Items, ApplyItem{Kind: "domain.issue_assignee", Name: spec.Domain.IssueAssignee, Action: "failed", Error: "未找到对应的 agent/squad"})
		}
	}
	// 6. Goals. goal_start (when set) overrides every template start flag —
	// the dialog's "start all now / keep all in backlog" switch.
	for _, g := range spec.Goals {
		assigneeType := g.AssigneeType
		if assigneeType == "" {
			assigneeType = "agent"
		}
		assigneeID := s.resolveAssigneeName(ctx, assigneeType, g.Assignee, res)
		start := g.Start
		if ov.GoalStart != nil {
			start = *ov.GoalStart
		}
		status := "backlog"
		if start {
			status = "active"
		}
		goal, err := s.goalSvc.Create(ctx, Goal{
			Title: g.Title, Description: g.Description,
			DomainID: createdDomain.ID, AssigneeType: assigneeType,
			AssigneeID: assigneeID, Status: status, CreatedByType: "human",
		})
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "goal", Name: g.Title, Action: "failed", Error: err.Error()})
			continue
		}
		res.Items = append(res.Items, ApplyItem{Kind: "goal", Name: g.Title, ID: goal.ID, Action: "created"})
		res.Goals = append(res.Goals, goal)
	}
	// 7. Schedules.
	for _, sc := range spec.Schedules {
		assigneeType := sc.AssigneeType
		if assigneeType == "" {
			assigneeType = "agent"
		}
		assigneeID := s.resolveAssigneeName(ctx, assigneeType, sc.Assignee, res)
		sch, err := s.schedSvc.Create(ctx, Schedule{
			Name: sc.Name, TitleTemplate: sc.Title, Description: sc.Description,
			AssigneeType: assigneeType, AssigneeID: assigneeID,
			DomainID: createdDomain.ID, CronExpression: sc.Cron,
			Timezone: localTimezone(), Enabled: true,
		})
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "schedule", Name: sc.Name, Action: "failed", Error: err.Error()})
			continue
		}
		res.Items = append(res.Items, ApplyItem{Kind: "schedule", Name: sc.Name, ID: sch.ID, Action: "created"})
		res.Schedules = append(res.Schedules, sch)
	}
	logging.Infof("template-apply: project template %s applied as domain %s (%d item(s))", templateID, createdDomain.ID, len(res.Items))
	return res, nil
}

// resolveIssueAssignee maps a template issue-assignee name (agent or squad)
// to an id — team members created by THIS apply resolve naturally (they are
// platform rows now); pre-existing entities resolve by name.
func (s *TemplateApplyService) resolveIssueAssignee(ctx context.Context, name, idType string) string {
	t := idType
	if t == "" {
		t = "agent"
	}
	switch t {
	case "agent":
		return s.agentIDByName(ctx, name)
	case "squad":
		if sq, err := s.squadByName(ctx, name); err == nil && sq != nil {
			return sq.ID
		}
	}
	return ""
}

// resolveAssigneeName maps a goal/schedule assignee name to an id. Template-
// created entities are platform rows by this point, so one name lookup path
// covers both. Unresolved names pass through — the service layer rejects
// them with its own validation message (surfaced per-item).
func (s *TemplateApplyService) resolveAssigneeName(ctx context.Context, assigneeType, name string, res *ApplyProjectResult) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	switch assigneeType {
	case "agent":
		if id := s.agentIDByName(ctx, name); id != "" {
			return id
		}
	case "squad":
		if sq, err := s.squadByName(ctx, name); err == nil && sq != nil {
			return sq.ID
		}
	}
	return name
}

// localTimezone mirrors the intake path: schedules speak the daemon's local
// time (the owner's wall clock on a single-user machine).
func localTimezone() string {
	tz := time.Local.String()
	if tz == "" {
		return "UTC"
	}
	return tz
}

func boolPtr(b bool) *bool { return &b }

// ParseTemplateForTest exposes parseTemplate to the validation harness
// (cmd-level tooling lives in package main and cannot reach internal
// unexported symbols otherwise).
func ParseTemplateForTest(raw string) (TemplateMeta, any, error) {
	return parseTemplate(raw)
}
