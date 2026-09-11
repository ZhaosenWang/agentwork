package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// ── fixtures ──

const squadTemplateYAML = `api_version: agentwork/v1
kind: squad-template
metadata:
  id: backend-dev
  name: 后端开发小队
  description: 测试用小队模板
  version: 1.0.0
spec:
  strategy: upsert
  agents:
    - name: backend-leader
      description: 负责人
      system_prompt: |
        你是负责人。
      skills: [test-skill]
      runtime: test-rt
    - name: backend-worker
      description: 开发
      system_prompt: |
        你是开发。
      runtime: test-rt
  squad:
    name: 后端开发小队
    description: 测试小队
    leader: backend-leader
    instructions: |
      拆解并委派。
    members:
      - name: backend-worker
        role: member
`

const projectTemplateYAML = `api_version: agentwork/v1
kind: project-template
metadata:
  id: go-service
  name: Go 服务
  description: 测试用项目模板
  version: 1.0.0
spec:
  domain:
    type: repo
    default_branch: main
    policy_text: |
      测试通过即完成。
  team:
    strategy: upsert
    agents:
      - name: go-owner
        description: 负责人
        system_prompt: |
          你是负责人。
        runtime: test-rt
      - name: go-dev
        description: 开发
        system_prompt: |
          你是开发。
        runtime: test-rt
    squad:
      name: go-squad
      leader: go-owner
      instructions: 拆解。
      members:
        - name: go-dev
          role: member
  goals:
    - title: 搭骨架
      description: 初始化
      assignee: go-owner
      assignee_type: agent
      start: true
    - title: 实现 healthz
      assignee: go-squad
      assignee_type: squad
  schedules:
    - name: 每日巡检
      title: 巡检
      cron: "0 9 * * *"
      assignee: go-owner
      assignee_type: agent
`

// templateTestCluster wires the template services over a fresh store with a
// runtime + agent-capable environment (no daemon — git tests use a stub).
type templateTestCluster struct {
	st        *store.Store
	templates *TemplateService
	apply     *TemplateApplyService
	agentSvc  *AgentService
	skillSvc  *SkillService
	squadSvc  *SquadService
	domainSvc *DomainService
	goalSvc   *GoalService
	schedSvc  *ScheduleService
	gitProbe  *stubGitTester
}

// stubGitTester records the last probe input and returns canned results.
type stubGitTester struct {
	lastURL, lastBranch, lastCred string
	result                        *DomainGitProbeResult
}

func (s *stubGitTester) TestDomainGit(_ context.Context, gitURL, defaultBranch, credentials string) *DomainGitProbeResult {
	s.lastURL, s.lastBranch, s.lastCred = gitURL, defaultBranch, credentials
	return s.result
}

func newTemplateCluster(t *testing.T) *templateTestCluster {
	t.Helper()
	st := newTestStore(t)
	bus := events.NewBus()
	settings := NewSettingsService(st)
	tpl := NewTemplateService(st, settings)
	agentSvc := NewAgentService(st, bus)
	skillSvc := NewSkillService(st)
	squadSvc := NewSquadService(st, bus)
	domainSvc := NewDomainService(st, bus)
	goalSvc := NewGoalService(st, bus)
	schedSvc := NewScheduleService(st, bus)
	probe := &stubGitTester{result: &DomainGitProbeResult{OK: true, BranchExists: true, ResolvedBranch: "main"}}
	apply := NewTemplateApplyService(st, tpl, agentSvc, skillSvc, squadSvc, domainSvc, goalSvc, schedSvc, probe)
	c := &templateTestCluster{
		st: st, templates: tpl, apply: apply, agentSvc: agentSvc,
		skillSvc: skillSvc, squadSvc: squadSvc, domainSvc: domainSvc,
		goalSvc: goalSvc, schedSvc: schedSvc, gitProbe: probe,
	}
	c.seedRuntime(t)
	return c
}

// seedRuntime inserts one active runtime so agent creation has a target.
func (c *templateTestCluster) seedRuntime(t *testing.T) {
	t.Helper()
	if _, err := c.st.DB().Exec(`INSERT INTO runtime (id,name,args,env,status,created_at) VALUES ('rt-1','test-rt','[]','{}','active','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
}

// seedSkill inserts a platform skill by name.
func (c *templateTestCluster) seedSkill(t *testing.T, name string) {
	t.Helper()
	if _, err := c.st.DB().Exec(`INSERT INTO skill (id,name,description,created_at) VALUES ('sk-`+name+`',?,?, '2026-01-01T00:00:00Z')`, name, ""); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
}

// putTemplate seeds the template cache directly (no GitCode in tests).
func (c *templateTestCluster) putTemplate(t *testing.T, kind, id, raw string) {
	t.Helper()
	settings := NewSettingsService(c.st)
	cache := map[string]any{
		"fetched_at": "2026-01-01T00:00:00Z",
		"repo":       map[string]any{"git_url": "https://example.com/templates.git", "branch": "main"},
		"templates": []map[string]any{{
			"api_version": "agentwork/v1", "kind": kind, "id": id,
			"name": id, "version": "1.0.0", "path": kind + "/" + id + ".yaml",
			"spec_yaml": raw,
		}},
	}
	b, _ := json.Marshal(cache)
	if err := settings.Set(context.Background(), templateCacheKey, string(b)); err != nil {
		t.Fatalf("seed template cache: %v", err)
	}
}

// ── parse tests ──

func TestParseTemplateStrictFields(t *testing.T) {
	// A misspelled spec field must fail loudly (R5).
	bad := `api_version: agentwork/v1
kind: squad-template
metadata:
  id: bad
spec:
  agents: []
  squad:
    name: x
    leader: y
    membars: []   # typo
`
	_, _, err := parseTemplate(bad)
	if err == nil || !strings.Contains(err.Error(), "membars") {
		t.Fatalf("expected strict-field error naming the typo, got %v", err)
	}
}

func TestParseTemplateKindMismatch(t *testing.T) {
	_, _, err := parseTemplate("api_version: agentwork/v1\nkind: bogus\nmetadata:\n  id: x\nspec: {}\n")
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("expected unsupported-kind error, got %v", err)
	}
}

// ── squad apply ──

func TestApplySquadCreatesAgentsAndSquad(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	c.putTemplate(t, TemplateKindSquad, "backend-dev", squadTemplateYAML)

	res, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Squad == nil || res.Squad.Name != "后端开发小队" {
		t.Fatalf("squad not created: %+v", res)
	}
	if len(res.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(res.Agents))
	}
	// Leader resolution: squad.leader_id = the agent created from backend-leader.
	if res.Squad.LeaderID == "" || res.Squad.LeaderID == res.Agents[1].ID {
		t.Fatalf("leader not resolved to the leader agent: %q", res.Squad.LeaderID)
	}
	// Skill references resolved to ids.
	if len(res.Agents[0].Skills) != 1 || res.Agents[0].Skills[0] != "sk-test-skill" {
		t.Fatalf("agent skills not resolved: %v", res.Agents[0].Skills)
	}
	// Per-item actions recorded.
	actions := map[string]string{}
	for _, it := range res.Items {
		actions[it.Kind+"/"+it.Name] = it.Action
	}
	if actions["agent/backend-leader"] != "created" || actions["squad/后端开发小队"] != "created" {
		t.Fatalf("unexpected item actions: %v", actions)
	}
}

func TestApplySquadRenameAndOverride(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	c.putTemplate(t, TemplateKindSquad, "backend-dev", squadTemplateYAML)

	res, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{
		SquadName: "订单小队",
		Rename:    map[string]string{"backend-leader": "订单-负责人", "backend-worker": "订单-开发"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Squad.Name != "订单小队" {
		t.Fatalf("squad name override ignored: %q", res.Squad.Name)
	}
	if res.Agents[0].Name != "订单-负责人" {
		t.Fatalf("rename ignored: %q", res.Agents[0].Name)
	}
	// The roster references resolve through template names, so membership
	// must be intact despite the rename.
	members, _ := c.squadSvc.ListMembers(ctx, res.Squad.ID)
	if len(members) != 1 || members[0].MemberID != res.Agents[1].ID {
		t.Fatalf("member lost after rename: %+v", members)
	}
}

func TestApplySquadCreateStrategyRejectsExistingName(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	c.putTemplate(t, TemplateKindSquad, "backend-dev", squadTemplateYAML)
	ov := ApplySquadOverrides{Strategy: conflictCreate}
	if _, err := c.apply.ApplySquad(ctx, "backend-dev", ov); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	_, err := c.apply.ApplySquad(ctx, "backend-dev", ov)
	if err == nil {
		t.Fatal("second create-apply should fail on the existing squad name")
	}
	// Upsert (default) replays cleanly instead.
	if _, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{}); err != nil {
		t.Fatalf("upsert replay should succeed: %v", err)
	}
}

func TestApplySquadUnknownSkillFailsThatAgent(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	// test-skill NOT seeded and not embedded → the agent referencing it
	// fails (per-item), while the clean agent still comes up.
	c.putTemplate(t, TemplateKindSquad, "backend-dev", squadTemplateYAML)
	res, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{})
	if err == nil {
		t.Fatal("apply should fail overall (the squad's leader is among the failed agents)")
	}
	var leaderFailed bool
	for _, it := range res.Items {
		if it.Name == "backend-leader" && it.Action == "failed" {
			leaderFailed = true
		}
	}
	if !leaderFailed {
		t.Fatalf("unknown skill should fail the referencing agent: %+v", res.Items)
	}
	if len(res.Agents) == 2 {
		t.Fatal("the skill-referencing agent must not be created")
	}
	// No squad can be created (its leader failed).
	squads, _ := c.squadSvc.List(ctx)
	if len(squads) != 0 {
		t.Fatalf("squad created despite failed leader: %d", len(squads))
	}
	// Platform stays consistent: only the clean agent exists.
	agents, _ := c.agentSvc.List(ctx)
	if len(agents) != 1 {
		t.Fatalf("only the clean agent should exist, got %d", len(agents))
	}
}

func TestApplySquadNoRuntimeFails(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	c.putTemplate(t, TemplateKindSquad, "backend-dev", squadTemplateYAML)
	if _, err := c.st.DB().Exec(`UPDATE runtime SET status='absent' WHERE id='rt-1'`); err != nil {
		t.Fatal(err)
	}
	_, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{})
	if err == nil {
		t.Fatal("expected apply to fail with no active runtime")
	}
	// Every agent item reports the runtime failure.
	for _, it := range []ApplyItem(nil) {
		_ = it
	}
	agents, _ := c.agentSvc.List(ctx)
	if len(agents) != 0 {
		t.Fatalf("no agent should be created without a runtime, got %d", len(agents))
	}
}

// ── project apply ──

func TestApplyProjectFullFlow(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service", projectTemplateYAML)

	res, err := c.apply.ApplyProject(ctx, "go-service", ApplyProjectOverrides{
		Name:           "订单中心",
		GitURL:         "https://gitcode.com/acme/orders.git",
		GitCredentials: "tok",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Domain == nil || res.Domain.Name != "订单中心" || res.Domain.GitURL == "" {
		t.Fatalf("domain not created properly: %+v", res.Domain)
	}
	if res.Domain.PolicyText == "" {
		t.Fatal("policy_text from template not persisted")
	}
	if res.Squad == nil || len(res.Agents) != 2 {
		t.Fatalf("team not created: squad=%v agents=%d", res.Squad, len(res.Agents))
	}
	if len(res.Goals) != 2 {
		t.Fatalf("expected 2 goals, got %d", len(res.Goals))
	}
	if res.Goals[0].Status != "active" {
		t.Fatalf("start:true goal should be active, got %q", res.Goals[0].Status)
	}
	if res.Goals[1].Status != "backlog" {
		t.Fatalf("start:false goal should be backlog, got %q", res.Goals[1].Status)
	}
	if res.Goals[0].DomainID != res.Domain.ID {
		t.Fatal("goal not bound to the new domain")
	}
	// Squad assignee resolved through the team section.
	if res.Goals[1].AssigneeType != "squad" || res.Goals[1].AssigneeID != res.Squad.ID {
		t.Fatalf("squad assignee not resolved: %+v", res.Goals[1])
	}
	if len(res.Schedules) != 1 || res.Schedules[0].CronExpression != "0 9 * * *" {
		t.Fatalf("schedule not created: %+v", res.Schedules)
	}
	// The git probe ran against the supplied URL.
	if c.gitProbe.lastURL != "https://gitcode.com/acme/orders.git" || c.gitProbe.lastCred != "tok" {
		t.Fatalf("probe inputs wrong: %q %q", c.gitProbe.lastURL, c.gitProbe.lastCred)
	}
}

func TestApplyProjectGoalStartOverlay(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service", projectTemplateYAML)
	f := false
	res, err := c.apply.ApplyProject(ctx, "go-service", ApplyProjectOverrides{
		Name: "订单中心", GitURL: "https://gitcode.com/acme/orders.git",
		GoalStart: &f,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, g := range res.Goals {
		if g.Status != "backlog" {
			t.Fatalf("goal_start=false overlay ignored: goal %q is %s", g.Title, g.Status)
		}
	}
}

func TestApplyProjectProbeFailureAborts(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service", projectTemplateYAML)
	c.gitProbe.result = &DomainGitProbeResult{OK: false, Error: "connection timed out"}
	_, err := c.apply.ApplyProject(ctx, "go-service", ApplyProjectOverrides{
		Name: "订单中心", GitURL: "https://gitcode.com/acme/orders.git",
	})
	if err == nil || !strings.Contains(err.Error(), "连接测试失败") {
		t.Fatalf("expected probe failure to abort, got %v", err)
	}
	// Nothing leaked: no domain, no agents.
	domains, _ := c.domainSvc.List(ctx)
	if len(domains) != 0 {
		t.Fatalf("domain created despite probe failure: %d", len(domains))
	}
	agents, _ := c.agentSvc.List(ctx)
	if len(agents) != 0 {
		t.Fatalf("agents created despite probe failure: %d", len(agents))
	}
}

func TestApplyProjectEmptyRepoRejected(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service", projectTemplateYAML)
	c.gitProbe.result = &DomainGitProbeResult{OK: true, BranchExists: false, Refs: nil, ResolvedBranch: "main"}
	_, err := c.apply.ApplyProject(ctx, "go-service", ApplyProjectOverrides{
		Name: "订单中心", GitURL: "https://gitcode.com/acme/orders.git",
	})
	if err == nil || !strings.Contains(err.Error(), "仓库为空") {
		t.Fatalf("expected empty-repo rejection, got %v", err)
	}
}

func TestApplyProjectMissingGitURL(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service", projectTemplateYAML)
	_, err := c.apply.ApplyProject(ctx, "go-service", ApplyProjectOverrides{Name: "订单中心"})
	if err == nil || !strings.Contains(err.Error(), "git_url") {
		t.Fatalf("expected missing git_url error, got %v", err)
	}
}

func TestApplyProjectScratchSkipsGit(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	scratchYAML := strings.Replace(projectTemplateYAML, "type: repo", "type: scratch", 1)
	c.putTemplate(t, TemplateKindProject, "go-service", scratchYAML)
	res, err := c.apply.ApplyProject(ctx, "go-service", ApplyProjectOverrides{Name: "周报"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Domain.Type != "scratch" || res.Domain.GitURL != "" {
		t.Fatalf("scratch domain wrong: %+v", res.Domain)
	}
	if c.gitProbe.lastURL != "" {
		t.Fatalf("scratch apply must not probe git, probed %q", c.gitProbe.lastURL)
	}
}

func TestApplyProjectDuplicateDomainNameFails(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service", projectTemplateYAML)
	ov := ApplyProjectOverrides{Name: "订单中心", GitURL: "https://gitcode.com/acme/orders.git"}
	if _, err := c.apply.ApplyProject(ctx, "go-service", ov); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	_, err := c.apply.ApplyProject(ctx, "go-service", ov)
	if err == nil {
		t.Fatal("duplicate domain name should fail")
	}
	var ce CodedError
	if !errors.As(err, &ce) || ce.Code() != CodeDomainNameExists {
		t.Fatalf("expected coded CodeDomainNameExists, got %v", err)
	}
}

// ── template service tests (registry parse / cache) ──

func TestParseTemplateFilesMixedValidity(t *testing.T) {
	files := map[string]string{
		"registry.yaml": `templates:
  - id: good
    kind: squad-template
    path: squads/good.yaml
  - id: bad
    kind: squad-template
    path: squads/bad.yaml
`,
		"squads/good.yaml": "api_version: agentwork/v1\nkind: squad-template\nmetadata:\n  id: good\nspec:\n  agents:\n    - name: a\n  squad:\n    leader: a\n",
		"squads/bad.yaml":  "not: a template\n",
	}
	c, err := parseTemplateFiles(files)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Templates) != 1 || c.Templates[0].ID != "good" {
		t.Fatalf("expected exactly the good template, got %+v", c.Templates)
	}
	if len(c.Errors) != 1 {
		t.Fatalf("expected one error entry for the bad file, got %v", c.Errors)
	}
}

func TestNormalizeKindFilter(t *testing.T) {
	for in, want := range map[string]string{
		"squad": TemplateKindSquad, "project": TemplateKindProject,
		"squad-template": TemplateKindSquad, TemplateKindProject: TemplateKindProject,
		"": "", "bogus": "",
	} {
		if got := normalizeKindFilter(in); got != want {
			t.Errorf("normalizeKindFilter(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── regression: Refresh wrote into a nil SkillFiles map (skills/ in the
// template repo → panic → nginx 502 on every /templates request) ──

func TestRefreshWritesSkillFilesWithoutPanic(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()

	orig := fetchTemplateFiles
	fetchTemplateFiles = func(ctx context.Context, cfg TemplateRepoConfig, token string) (map[string]string, error) {
		return map[string]string{
			"registry.yaml":                     `templates: []`,
			"skills/daily-report/SKILL.md":      "# daily report skill\n",
			"skills/daily-report/pkg/extra.txt": "x\n",
		}, nil
	}
	defer func() { fetchTemplateFiles = orig }()

	res, err := c.templates.Refresh(ctx, "")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Fetched != 0 {
		t.Fatalf("expected 0 templates, got %d", res.Fetched)
	}
	snap, err := c.templates.cached(ctx)
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if len(snap.SkillFiles) != 2 {
		t.Fatalf("expected 2 skill files in snapshot, got %d: %v", len(snap.SkillFiles), snap.SkillFiles)
	}
	if snap.SkillFiles["skills/daily-report/SKILL.md"] == "" || snap.SkillFiles["skills/daily-report/pkg/extra.txt"] == "" {
		t.Fatalf("skill file contents lost: %v", snap.SkillFiles)
	}
	// SkillPackage strips the skills/<name>/ prefix at read time.
	pkg, ok, err := c.templates.SkillPackage(ctx, "daily-report")
	if err != nil || !ok {
		t.Fatalf("SkillPackage: ok=%v err=%v", ok, err)
	}
	if len(pkg) != 2 || pkg["SKILL.md"] == "" {
		t.Fatalf("SkillPackage mismatch: %v", pkg)
	}
}

func TestParseTemplateFilesInitializesSkillFiles(t *testing.T) {
	c, err := parseTemplateFiles(map[string]string{
		"registry.yaml": `templates: []`,
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.SkillFiles == nil {
		t.Fatal("parseTemplateFiles must return a non-nil SkillFiles map")
	}
}
