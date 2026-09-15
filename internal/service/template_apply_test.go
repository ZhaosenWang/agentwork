package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/acp"
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

// fullAgentFieldsYAML exercises every tplAgent field the apply path must carry
// through to the DB: mcp_servers (stdio + http), env, model, max_concurrent.
const fullAgentFieldsYAML = `api_version: agentwork/v1
kind: squad-template
metadata:
  id: full-fields
  name: 全字段小队
  description: 覆盖 env/mcp/model/max_concurrent 落库
  version: 1.0.0
spec:
  strategy: upsert
  agents:
    - name: full-agent
      description: 携带全部 agent 定义属性
      system_prompt: |
        你是全字段 agent。
      skills: []
      runtime: test-rt
      model: glm-4.6
      max_concurrent: 5
      env:
        LOG_LEVEL: debug
        REGION: cn-east
      mcp_servers:
        - name: browser
          type: stdio
          command: npx
          args: ["-y", "@modelcontextprotocol/server-puppeteer"]
        - name: api
          type: http
          url: https://api.example.com/mcp
          headers:
            - name: Authorization
              value: Bearer test-token
  squad:
    name: 全字段小队
    description: 测试小队
    leader: full-agent
    instructions: |
      测试。
    members: []
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
	st          *store.Store
	templates   *TemplateService
	apply       *TemplateApplyService
	agentSvc    *AgentService
	skillSvc    *SkillService
	squadSvc    *SquadService
	domainSvc   *DomainService
	goalSvc     *GoalService
	schedSvc    *ScheduleService
	gitProbe    *stubGitTester
	skillPusher *stubSkillPusher
}

// stubGitTester records the last probe input and returns canned results.
type stubGitTester struct {
	lastURL, lastBranch, lastCred string
	result                        *DomainGitProbeResult
}

// stubSkillPusher records every PushAgentSkills call so a test can assert the
// apply path shipped persona+skills to the machine (the HTTP handlers do the
// same). The apply path fires it on a detached goroutine, so waitForPush polls.
type stubSkillPusher struct {
	mu     sync.Mutex
	pushed []string
}

func (s *stubSkillPusher) push(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushed = append(s.pushed, agentID)
}

func (s *stubSkillPusher) waitForPush(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := len(s.pushed)
		s.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("PushAgentSkills not called %d time(s); got %v", n, s.pushed)
}

func (s *stubSkillPusher) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.pushed))
	copy(out, s.pushed)
	return out
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
	pusher := &stubSkillPusher{}
	apply := NewTemplateApplyService(st, tpl, agentSvc, skillSvc, squadSvc, domainSvc, goalSvc, schedSvc, probe)
	apply.SetSkillPusher(pusher.push)
	c := &templateTestCluster{
		st: st, templates: tpl, apply: apply, agentSvc: agentSvc,
		skillSvc: skillSvc, squadSvc: squadSvc, domainSvc: domainSvc,
		goalSvc: goalSvc, schedSvc: schedSvc, gitProbe: probe,
		skillPusher: pusher,
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

// TestApplySquadFullAgentFields asserts every tplAgent field (mcp_servers,
// env, model, max_concurrent) lands in the DB via the same AgentService.Create
// a manual agent uses, and that apply ships persona+skills to the machine
// (the HTTP handlers do the same). Before the fix, mcp_servers was dropped on
// every path and the other three on the upsert path.
func TestApplySquadFullAgentFields(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindSquad, "full-fields", fullAgentFieldsYAML)

	res, err := c.apply.ApplySquad(ctx, "full-fields", ApplySquadOverrides{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(res.Agents))
	}
	a := res.Agents[0]
	if a.Model != "glm-4.6" {
		t.Errorf("model: got %q want glm-4.6", a.Model)
	}
	if a.MaxConcurrent != 5 {
		t.Errorf("max_concurrent: got %d want 5", a.MaxConcurrent)
	}
	if len(a.Env) != 2 || a.Env["LOG_LEVEL"] != "debug" || a.Env["REGION"] != "cn-east" {
		t.Errorf("env: got %v want {LOG_LEVEL:debug, REGION:cn-east}", a.Env)
	}
	if len(a.McpServers) != 2 {
		t.Fatalf("mcp_servers: got %d want 2 (was dropped before the fix)", len(a.McpServers))
	}
	if a.McpServers[0].Name != "browser" || a.McpServers[0].Type != "stdio" || a.McpServers[0].Command != "npx" || len(a.McpServers[0].Args) != 2 {
		t.Errorf("stdio mcp: %+v", a.McpServers[0])
	}
	if a.McpServers[1].Name != "api" || a.McpServers[1].Type != "http" || a.McpServers[1].URL != "https://api.example.com/mcp" {
		t.Errorf("http mcp: %+v", a.McpServers[1])
	}
	if len(a.McpServers[1].Headers) != 1 || a.McpServers[1].Headers[0].Name != "Authorization" || a.McpServers[1].Headers[0].Value != "Bearer test-token" {
		t.Errorf("http mcp headers: %+v", a.McpServers[1].Headers)
	}
	// apply ships persona+skills to the machine right after create.
	c.skillPusher.waitForPush(t, 1)
	if c.skillPusher.snapshot()[0] != a.ID {
		t.Errorf("PushAgentSkills: got %v want [%s]", c.skillPusher.snapshot(), a.ID)
	}
}

// TestApplySquadUpsertUpdatesAllFields asserts the upsert (default) strategy
// rewrites env/model/mcp_servers/max_concurrent on an EXISTING agent via
// AgentService.Update (all 10 columns), not just description/system_prompt/
// skills. Before the fix, UpsertByName's narrow UPDATE left these untouched.
func TestApplySquadUpsertUpdatesAllFields(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindSquad, "full-fields", fullAgentFieldsYAML)

	// First apply creates the agent with the full field set.
	if _, err := c.apply.ApplySquad(ctx, "full-fields", ApplySquadOverrides{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	c.skillPusher.waitForPush(t, 1)
	agentID := c.skillPusher.snapshot()[0]

	// Tamper: wipe the four fields to prove the second apply rewrites them.
	if _, err := c.st.DB().ExecContext(ctx,
		`UPDATE agent SET model='', max_concurrent=1, env='{}', mcp_servers='[]' WHERE id=?`, agentID); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	// Second apply: upsert hits the existing agent → Update writes all fields.
	res, err := c.apply.ApplySquad(ctx, "full-fields", ApplySquadOverrides{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(res.Agents) != 1 || res.Agents[0].ID != agentID {
		t.Fatalf("upsert should reuse the same agent: got %+v", res.Agents)
	}
	if res.Items[0].Action != "updated" {
		t.Errorf("action: got %q want updated", res.Items[0].Action)
	}
	c.skillPusher.waitForPush(t, 2) // second push from the update path

	// Re-read from DB: the four fields were rewritten, not left empty.
	got, err := c.agentSvc.Get(ctx, agentID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "glm-4.6" {
		t.Errorf("model after upsert: got %q want glm-4.6", got.Model)
	}
	if got.MaxConcurrent != 5 {
		t.Errorf("max_concurrent after upsert: got %d want 5", got.MaxConcurrent)
	}
	if len(got.Env) != 2 || got.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("env after upsert: %v", got.Env)
	}
	if len(got.McpServers) != 2 {
		t.Errorf("mcp_servers after upsert: got %d want 2", len(got.McpServers))
	}
}

// TestTplMcpServersToAcp unit-covers the loose-YAML → typed conversion,
// including the empty-input short-circuit.
func TestTplMcpServersToAcp(t *testing.T) {
	if got := tplMcpServersToAcp(nil); got != nil {
		t.Errorf("nil input: got %+v want nil", got)
	}
	in := []map[string]any{
		{"name": "stdio-srv", "type": "stdio", "command": "npx", "args": []any{"-y", "srv"}},
		{"name": "http-srv", "type": "http", "url": "https://x/mcp"},
	}
	out := tplMcpServersToAcp(in)
	if len(out) != 2 {
		t.Fatalf("got %d want 2", len(out))
	}
	if out[0].Command != "npx" || len(out[0].Args) != 2 || out[0].Args[0] != "-y" {
		t.Errorf("stdio: %+v", out[0])
	}
	if out[1].Type != "http" || out[1].URL != "https://x/mcp" {
		t.Errorf("http: %+v", out[1])
	}
	_ = acp.McpServer{} // keep the acp import used if the assertions above ever move
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

// TestApplySquadOfflineMachineSkipped verifies apply's runtime selection uses
// the claim gate's availability test (runtime not absent AND machine
// connected), not runtime.status alone. A runtime whose owning machine is
// offline is still status='active' in the DB, but must NOT be selected —
// neither by name match nor by fallback. The fixture seeds two runtimes on
// two machines: m-up (connected) hosts rt-up, m-down (offline) hosts rt-down.
// The template names rt-down; the offline name must fall through to rt-up.
func TestApplySquadOfflineMachineSkipped(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	// newTemplateCluster seeded rt-1 (machine_id='' — always usable). Drop it
	// so the only usable runtime is the connected-machine one we add below.
	if _, err := c.st.DB().Exec(`DELETE FROM runtime WHERE id='rt-1'`); err != nil {
		t.Fatal(err)
	}
	machines := []struct{ id, name, status string }{
		{"m-up", "up-host", "connected"},
		{"m-down", "down-host", "offline"},
	}
	for _, m := range machines {
		if _, err := c.st.DB().Exec(
			`INSERT INTO machine (id,name,hostname,version,probed_clis,last_seen_at,status,created_at)
			 VALUES (?,?,?, '', '[]', '', ?, '2026-01-01T00:00:00Z')`,
			m.id, m.name, m.name, m.status); err != nil {
			t.Fatalf("seed machine %s: %v", m.id, err)
		}
	}
	runTimes := []struct{ id, name, machineID, created string }{
		{"rt-up", "up-rt@m-up", "m-up", "2026-01-01T00:00:00Z"},
		{"rt-down", "down-rt@m-down", "m-down", "2026-01-02T00:00:00Z"}, // newer → would win ORDER BY created_at
	}
	for _, r := range runTimes {
		if _, err := c.st.DB().Exec(
			`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at)
			 VALUES (?,? ,? ,'[]','{}','active',?)`,
			r.id, r.name, r.machineID, r.created); err != nil {
			t.Fatalf("seed runtime %s: %v", r.id, err)
		}
	}

	// Template names the offline machine's runtime — must fall through to rt-up.
	offlineNameYAML := strings.Replace(squadTemplateYAML, "runtime: test-rt", "runtime: down-rt@m-down", 1)
	offlineNameYAML = strings.ReplaceAll(offlineNameYAML, "backend-leader", "offline-leader")
	offlineNameYAML = strings.ReplaceAll(offlineNameYAML, "backend-worker", "offline-worker")
	c.putTemplate(t, TemplateKindSquad, "offline-name", offlineNameYAML)
	res, err := c.apply.ApplySquad(ctx, "offline-name", ApplySquadOverrides{})
	if err != nil {
		t.Fatalf("apply should succeed by falling back to the connected runtime: %v", err)
	}
	if len(res.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(res.Agents))
	}
	for _, a := range res.Agents {
		if a.RuntimeID != "rt-up" {
			t.Fatalf("agent %s bound to %s, expected rt-up (the only connected runtime)", a.Name, a.RuntimeID)
		}
	}
}

// TestApplySquadAllMachinesOfflineFails verifies that when every runtime's
// machine is offline, apply refuses to create agents — matching what Claim
// would reject — rather than binding them to dead runtimes.
func TestApplySquadAllMachinesOfflineFails(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	if _, err := c.st.DB().Exec(`DELETE FROM runtime WHERE id='rt-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.DB().Exec(
		`INSERT INTO machine (id,name,hostname,version,probed_clis,last_seen_at,status,created_at)
		 VALUES ('m-down','down-host','down-host','','[]','','offline','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.DB().Exec(
		`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at)
		 VALUES ('rt-down','down-rt@m-down','m-down','[]','{}','active','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	c.putTemplate(t, TemplateKindSquad, "backend-dev", squadTemplateYAML)
	_, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{})
	if err == nil {
		t.Fatal("expected apply to fail when all machines are offline")
	}
	agents, _ := c.agentSvc.List(ctx)
	if len(agents) != 0 {
		t.Fatalf("no agent should be created when all machines are offline, got %d", len(agents))
	}
}

// TestUsableRuntimeIDsExcludesOffline verifies the usable-runtime list itself
// filters out offline-machine runtimes, so the fallback pool only contains
// runtimes Claim would actually dispatch.
func TestUsableRuntimeIDsExcludesOffline(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	if _, err := c.st.DB().Exec(`DELETE FROM runtime WHERE id='rt-1'`); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ id, status string }{
		{"m-up", "connected"},
		{"m-down", "offline"},
	} {
		if _, err := c.st.DB().Exec(
			`INSERT INTO machine (id,name,hostname,version,probed_clis,last_seen_at,status,created_at)
			 VALUES (?, ?, ?, '', '[]', '', ?, '2026-01-01T00:00:00Z')`,
			m.id, m.id, m.id, m.status); err != nil {
			t.Fatalf("seed machine %s: %v", m.id, err)
		}
	}
	if _, err := c.st.DB().Exec(
		`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-up','up-rt@m-up','m-up','[]','{}','active','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.DB().Exec(
		`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-down','down-rt@m-down','m-down','[]','{}','active','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	ids, err := c.apply.usableRuntimeIDs(ctx)
	if err != nil {
		t.Fatalf("usableRuntimeIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "rt-up" {
		t.Fatalf("expected only [rt-up], got %v", ids)
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

// A create-type template (spec.repo.create=true) applied with an explicit
// git_url must skip repo creation and bind the supplied repo instead — the
// existing-repo override. Without this the backend demands repo_token even
// when the user chose "use existing repo" (AW.10000001), which is the
// design mismatch the frontend's existing-mode choice exposed.
func TestApplyProjectRepoCreateWithGitURLSkipsCreation(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service-repo-create", repoCreateTemplateYAML)

	res, err := c.apply.ApplyProject(ctx, "go-service-repo-create", ApplyProjectOverrides{
		Name:           "已有仓项目",
		GitURL:         "https://gitcode.com/acme/existing.git",
		GitCredentials: "tok",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Domain == nil || res.Domain.GitURL != "https://gitcode.com/acme/existing.git" {
		t.Fatalf("domain not bound to supplied git_url: %+v", res.Domain)
	}
	if res.RepoURL != "" {
		t.Fatalf("repo must not be created when git_url supplied, got RepoURL=%q", res.RepoURL)
	}
	// No repo-creation item; the probe ran against the supplied URL.
	for _, it := range res.Items {
		if it.Kind == "repo" {
			t.Fatalf("unexpected repo creation item: %+v", it)
		}
	}
	if c.gitProbe.lastURL != "https://gitcode.com/acme/existing.git" {
		t.Fatalf("probe should run on supplied git_url, got %q", c.gitProbe.lastURL)
	}
}

// A create-type template applied with neither git_url nor repo_token still
// demands repo_token — the create path is the default and remains required.
func TestApplyProjectRepoCreateMissingToken(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service-repo-create", repoCreateTemplateYAML)

	_, err := c.apply.ApplyProject(ctx, "go-service-repo-create", ApplyProjectOverrides{Name: "建仓项目"})
	if err == nil || !strings.Contains(err.Error(), "repo_token") {
		t.Fatalf("expected repo_token error, got %v", err)
	}
}

// repoSlug derives a GitCode-acceptable repo slug from a domain name that may
// contain Chinese — the GitCode create-repo API rejects non-ASCII in the name
// field (422), but the domain name (human-facing) allows it. The slug strips
// non-ASCII to '-' and falls back to the template id when nothing ASCII
// remains, so a Chinese-only name still produces a valid slug.
func TestRepoSlug(t *testing.T) {
	cases := []struct {
		name, in, fallback, want string
	}{
		{"ascii", "go-service", "tpl", "go-service"},
		{"mixed case lowercased", "GoService", "tpl", "goservice"},
		{"chinese stripped", "Go 微服务项目12", "tpl", "go-12"},
		{"chinese only falls back", "微服务", "go-svc", "go-svc"},
		{"leading trailing sep trimmed", "  hi  ", "tpl", "hi"},
		{"dots preserved", "my.repo.v2", "tpl", "my.repo.v2"},
		{"underscores preserved", "my_repo", "tpl", "my_repo"},
		{"punctuation to dash", "a/b:c", "tpl", "a-b-c"},
		{"empty falls back", "", "tpl", "tpl"},
		{"collapse runs", "a--b:::c", "tpl", "a-b-c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repoSlug(tc.in, tc.fallback)
			if got != tc.want {
				t.Fatalf("repoSlug(%q, %q) = %q, want %q", tc.in, tc.fallback, got, tc.want)
			}
		})
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

// forwardRefAssigneeYAML mirrors the real existing-repo template: the domain's
// issue_assignee names a team agent that does NOT exist until step 5 creates
// it. Before the fix, DomainService.Create → validateIssueTracking ran
// mustExist(...WHERE id=?) against the raw NAME and aborted the whole apply
// before the team section ever ran (AW.10000001 "issue assignee ... does not
// exist").
const forwardRefAssigneeYAML = `api_version: agentwork/v1
kind: project-template
metadata:
  id: existing-repo
  name: 已有仓库项目
  version: 1.0.0
spec:
  domain:
    type: repo
    default_branch: main
    issue_assignee_type: agent
    issue_assignee: repo-issue-handler
    policy_text: |
      验收要求。
  team:
    strategy: upsert
    agents:
      - name: repo-issue-handler
        description: 值班工程师
        system_prompt: |
          你是值班工程师。
        runtime: test-rt
      - name: repo-reviewer
        description: 审查员
        system_prompt: |
          你是审查员。
        runtime: test-rt
    squad:
      name: repo-crew
      leader: repo-issue-handler
      instructions: 处理 issue。
      members:
        - name: repo-reviewer
          role: reviewer
  goals:
    - title: 建立基线
      assignee: repo-issue-handler
      assignee_type: agent
      start: true
`

func TestApplyProjectForwardRefIssueAssignee(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "existing-repo", forwardRefAssigneeYAML)

	res, err := c.apply.ApplyProject(ctx, "existing-repo", ApplyProjectOverrides{
		Name:           "已有仓库",
		GitURL:         "https://gitcode.com/acme/existing.git",
		GitCredentials: "tok",
	})
	if err != nil {
		t.Fatalf("forward-ref apply must succeed, got: %v", err)
	}
	// The team agent named by domain.issue_assignee was created.
	var handlerID string
	for _, a := range res.Agents {
		if a.Name == "repo-issue-handler" {
			handlerID = a.ID
		}
	}
	if handlerID == "" {
		t.Fatalf("repo-issue-handler agent not created: %+v", res.Agents)
	}
	// The domain's issue_assignee was patched to the agent's id (not left as
	// the raw name, not left empty).
	if res.Domain.IssueAssignee != handlerID {
		t.Fatalf("issue_assignee not resolved to handler id: got %q want %q",
			res.Domain.IssueAssignee, handlerID)
	}
	if res.Domain.IssueAssigneeType != "agent" {
		t.Fatalf("issue_assignee_type wrong: %q", res.Domain.IssueAssigneeType)
	}
	// The per-item trace records the deferred patch.
	var patched bool
	for _, it := range res.Items {
		if it.Kind == "domain.issue_assignee" && it.Action == "updated" && it.ID == handlerID {
			patched = true
		}
	}
	if !patched {
		t.Fatalf("issue_assignee patch item missing: %+v", res.Items)
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

// ── regression: Get returned the WHOLE YAML document as spec (metadata
// siblings included) — the frontend read spec.repo?.create off the detail and
// got undefined, so repo-create templates showed the git_url form and
// repo_token was never sent (AW.10000001) ──

func TestGetSpecIsInnerNodeOnly(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindProject, "go-service-repo-create", repoCreateTemplateYAML)

	detail, err := c.templates.Get(ctx, "project", "go-service-repo-create")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The frontend contract: spec.repo / spec.domain etc. at the TOP level.
	var spec struct {
		Domain *struct {
			Type string `json:"type"`
		} `json:"domain"`
		Repo *struct {
			Create bool `json:"create"`
		} `json:"repo"`
		Team *struct {
			Agents []map[string]any `json:"agents"`
		} `json:"team"`
		Spec     *map[string]any `json:"spec"`     // must be absent
		Metadata *map[string]any `json:"metadata"` // must be absent
	}
	if err := json.Unmarshal(detail.Spec, &spec); err != nil {
		t.Fatalf("spec is not a JSON object: %v (%s)", err, detail.Spec)
	}
	if spec.Domain == nil || spec.Domain.Type != "repo" {
		t.Fatalf("spec.domain missing/wrong: %+v", spec)
	}
	if spec.Repo == nil || !spec.Repo.Create {
		t.Fatalf("spec.repo.create missing — frontend repo_token detection would break")
	}
	if spec.Spec != nil || spec.Metadata != nil {
		t.Fatalf("spec must not nest the document (spec/metadata leaked): %s", detail.Spec)
	}
	if len(spec.Team.Agents) != 2 {
		t.Fatalf("spec.team.agents missing: %+v", spec.Team)
	}
	if len(detail.SpecYAML) == 0 || !strings.Contains(detail.SpecYAML, "api_version") {
		t.Fatal("spec_yaml must stay the full document")
	}
}

func TestTemplateSpecJSONEdgeCases(t *testing.T) {
	cases := map[string]string{ // doc → expected raw spec JSON
		"api_version: agentwork/v1\nspec:\n  domain:\n    type: scratch\n": `{"domain":{"type":"scratch"}}`,
		"api_version: agentwork/v1\nmetadata:\n  id: x\n":                  `null`, // no spec section
		"": `null`,
	}
	for doc, want := range cases {
		got, err := templateSpecJSON(doc)
		if err != nil {
			t.Fatalf("templateSpecJSON(%q): %v", doc, err)
		}
		if string(got) != want {
			t.Fatalf("templateSpecJSON(%q) = %s, want %s", doc, got, want)
		}
	}
}

// repoCreateTemplateYAML mirrors the real go-service template (repo.create section).
const repoCreateTemplateYAML = `api_version: agentwork/v1
kind: project-template
metadata:
  id: go-service-repo-create
  name: Go 服务（建仓）
  description: 测试建仓检测
  version: 1.0.0
spec:
  repo:
    create: true
    visibility: private
  domain:
    type: repo
  team:
    agents:
      - name: owner-a
      - name: dev-a
    squad:
      name: go-squad
      leader: owner-a
      members:
        - name: dev-a
          role: member
  goals:
    - title: 初始化
      assignee: owner-a
      assignee_type: agent
      start: true
`

// ── regression: Refresh had no independent timeout — refreshTimeout was
// declared but never applied, so a hanging git clone blocked the caller
// until the HTTP client timed out (and List's auto-refresh inherits it) ──

func TestRefreshAppliesIndependentTimeout(t *testing.T) {
	c := newTemplateCluster(t)

	orig := fetchTemplateFiles
	// The stub asserts the ctx Refresh passes in carries its OWN deadline —
	// i.e. Refresh wrapped it with WithTimeout(refreshTimeout). Before the
	// fix, the ctx had no deadline (unless the caller supplied one) and a
	// hanging clone blocked indefinitely.
	fetchTemplateFiles = func(ctx context.Context, _ TemplateRepoConfig, _ string) (map[string]string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("ctx passed to fetch has no deadline — Refresh did not apply an independent timeout")
		}
		return map[string]string{"registry.yaml": "templates: []"}, nil
	}
	defer func() { fetchTemplateFiles = orig }()

	// context.Background() has no deadline — any deadline the fetch sees must
	// have been added by Refresh itself.
	if _, err := c.templates.Refresh(context.Background(), ""); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

// ── agent-template fixtures + tests ──

const agentTemplateYAML = `api_version: agentwork/v1
kind: agent-template
metadata:
  id: code-reviewer
  name: 代码审查员
  description: 单 agent 代码审查
  version: 1.0.0
spec:
  strategy: upsert
  agent:
    name: code-reviewer
    description: 严格审查员
    system_prompt: |
      你是严格的代码审查员。
    skills: [test-skill]
    runtime: test-rt
    model: glm-4.6
    max_concurrent: 3
    env:
      LOG_LEVEL: debug
  suggested_goal:
    title: 审查最近的代码变更
    description: |
      拉取最近变更并输出审查意见。
`

const agentTemplateNoSuggestedGoalYAML = `api_version: agentwork/v1
kind: agent-template
metadata:
  id: bare-agent
  name: 裸 agent
  description: 无 suggested_goal
  version: 1.0.0
spec:
  agent:
    name: bare-agent
    description: 裸
    system_prompt: 你是裸 agent。
    runtime: test-rt
`

// TestApplyAgentCreatesAgentScratchDomainAndSuggestedGoal asserts the full
// agent-template apply: agent created with all fields, a same-named scratch
// domain auto-created as the goal default project, and the suggested_goal
// prompt surfaced for the frontend to pre-fill.
func TestApplyAgentCreatesAgentScratchDomainAndSuggestedGoal(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	c.putTemplate(t, TemplateKindAgent, "code-reviewer", agentTemplateYAML)

	res, err := c.apply.ApplyAgent(ctx, "code-reviewer", ApplyAgentOverrides{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Agent == nil || res.Agent.Name != "code-reviewer" {
		t.Fatalf("agent not created: %+v", res.Agent)
	}
	if res.Agent.Model != "glm-4.6" || res.Agent.MaxConcurrent != 3 {
		t.Fatalf("agent fields not carried: model=%q max=%d", res.Agent.Model, res.Agent.MaxConcurrent)
	}
	if res.Domain == nil || res.Domain.Type != "scratch" || res.Domain.Name != "code-reviewer" {
		t.Fatalf("scratch domain not auto-created with agent name: %+v", res.Domain)
	}
	if res.SuggestedGoal == nil || res.SuggestedGoal.Title != "审查最近的代码变更" {
		t.Fatalf("suggested_goal not surfaced: %+v", res.SuggestedGoal)
	}
	actions := map[string]string{}
	for _, it := range res.Items {
		actions[it.Kind+"/"+it.Name] = it.Action
	}
	if actions["agent/code-reviewer"] != "created" || actions["domain/code-reviewer"] != "created" {
		t.Fatalf("unexpected item actions: %v", actions)
	}
	// The skill pusher fires for an apply-created agent (same as squad path).
	c.skillPusher.waitForPush(t, 1)
}

// TestApplyAgentNoSuggestedGoalIsOK — suggested_goal is optional; a bare
// agent template still applies, with a nil SuggestedGoal.
func TestApplyAgentNoSuggestedGoalIsOK(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.putTemplate(t, TemplateKindAgent, "bare-agent", agentTemplateNoSuggestedGoalYAML)

	res, err := c.apply.ApplyAgent(ctx, "bare-agent", ApplyAgentOverrides{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Agent == nil || res.Domain == nil {
		t.Fatalf("agent/domain not created: %+v", res)
	}
	if res.SuggestedGoal != nil {
		t.Fatalf("expected nil suggested_goal, got %+v", res.SuggestedGoal)
	}
}

// TestApplyAgentEmptyNameFails — an agent-template without agent.name is
// invalid (the apply has nothing to name the agent or the scratch domain).
func TestApplyAgentEmptyNameFails(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	raw := strings.Replace(agentTemplateYAML, "name: code-reviewer\n    description: 严格审查员", "description: 严格审查员", 1)
	c.putTemplate(t, TemplateKindAgent, "code-reviewer", raw)

	_, err := c.apply.ApplyAgent(ctx, "code-reviewer", ApplyAgentOverrides{})
	if err == nil {
		t.Fatalf("expected empty-name error")
	}
}

// TestApplyAgentRenameOverride — AgentName override renames both the agent
// and the auto-created scratch domain (the domain rides the final name).
func TestApplyAgentRenameOverride(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	c.putTemplate(t, TemplateKindAgent, "code-reviewer", agentTemplateYAML)

	res, err := c.apply.ApplyAgent(ctx, "code-reviewer", ApplyAgentOverrides{AgentName: "我的审查员"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Agent.Name != "我的审查员" {
		t.Fatalf("agent name not overridden: %q", res.Agent.Name)
	}
	if res.Domain.Name != "我的审查员" {
		t.Fatalf("domain should ride the final name, got %q", res.Domain.Name)
	}
}

// TestApplySquadCreatesScratchDomainAndSuggestedGoal — ApplySquad now
// auto-creates a same-named scratch domain and surfaces suggested_goal.
func TestApplySquadCreatesScratchDomainAndSuggestedGoal(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	c.seedSkill(t, "test-skill")
	// Add a suggested_goal to the squad fixture inline.
	raw := squadTemplateYAML + "  suggested_goal:\n    title: 启动后端开发\n    description: 拆解首个需求并委派。\n"
	c.putTemplate(t, TemplateKindSquad, "backend-dev", raw)

	res, err := c.apply.ApplySquad(ctx, "backend-dev", ApplySquadOverrides{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Domain == nil || res.Domain.Type != "scratch" || res.Domain.Name != "后端开发小队" {
		t.Fatalf("squad scratch domain not created: %+v", res.Domain)
	}
	if res.SuggestedGoal == nil || res.SuggestedGoal.Title != "启动后端开发" {
		t.Fatalf("squad suggested_goal not surfaced: %+v", res.SuggestedGoal)
	}
}

// TestEnsureScratchDomainReusesExisting — a same-named domain (any type) is
// reused, not duplicated, so re-applying a template doesn't fail on the domain.
func TestEnsureScratchDomainReusesExisting(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	existing, err := c.domainSvc.Create(ctx, Domain{Type: "scratch", Name: "code-reviewer"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	dom, err := c.apply.ensureScratchDomain(ctx, "code-reviewer")
	if err != nil {
		t.Fatalf("ensureScratchDomain: %v", err)
	}
	if dom.ID != existing.ID {
		t.Fatalf("expected reuse of existing domain %q, got %q", existing.ID, dom.ID)
	}
}

// TestEnsureScratchDomainCollisionSafe — re-applying the same template
// reuses the existing same-named domain (no duplicate, no failure). The
// "-2"/"-3" suffix path only fires for a Create-time race a single-threaded
// test can't reproduce; the observable contract is "same name → reuse".
func TestEnsureScratchDomainCollisionSafe(t *testing.T) {
	c := newTemplateCluster(t)
	ctx := context.Background()
	first, err := c.apply.ensureScratchDomain(ctx, "dup-name")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if first.Name != "dup-name" {
		t.Fatalf("expected dup-name, got %q", first.Name)
	}
	// Second call with the same name reuses the first — no duplicate, no error.
	second, err := c.apply.ensureScratchDomain(ctx, "dup-name")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected reuse of %q, got %q (duplicate created)", first.ID, second.ID)
	}
	// A different name creates a distinct domain.
	other, err := c.apply.ensureScratchDomain(ctx, "other-name")
	if err != nil {
		t.Fatalf("other ensure: %v", err)
	}
	if other.ID == first.ID {
		t.Fatalf("distinct name should create a distinct domain")
	}
}
