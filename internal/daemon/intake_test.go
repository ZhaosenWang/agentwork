package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// newIntakeDaemon builds the daemon surfaces the intake executor needs: a
// real store with an agent + a gated domain, and the goal/run services.
func newIntakeDaemon(t *testing.T) (*Daemon, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,created_at) VALUES ('rt1','rt1',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a1','worker1','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	ds := service.NewDomainService(st, bus)
	if _, err := ds.Create(ctx, service.Domain{Name: "d1", GitURL: "https://e.com/d1.git"}); err != nil {
		t.Fatal(err)
	}
	schedSvc := service.NewScheduleService(st, bus)
	agentSvc := service.NewAgentService(st, bus)
	squadSvc := service.NewSquadService(st, bus)
	intakeSvc := notify.NewIntakeService(notify.NewSQLQueryStore(st), &mapSettings{vals: map[string]string{}}, runSvc)
	skillSvc := service.NewSkillService(st)
	d := &Daemon{
		st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc, schedSvc: schedSvc,
		agentSvc: agentSvc, squadSvc: squadSvc, qs: notify.NewSQLQueryStore(st),
		intakeSvc: intakeSvc, domainSvc: ds, skillSvc: skillSvc,
	}
	return d, st
}

// mapSettings is a SettingsStore fake for tests (mirrors notify/card_test.go).
type mapSettings struct {
	vals map[string]string
}

func (m *mapSettings) Get(_ context.Context, key string) (string, error) {
	return m.vals[key], nil
}
func (m *mapSettings) Set(_ context.Context, key, value string) error {
	m.vals[key] = value
	return nil
}
func (m *mapSettings) Delete(_ context.Context, key string) error {
	delete(m.vals, key)
	return nil
}

// TestIntakeCreateGoal: the platform executes the parsed create action
// through the goal layer — active goal created, first run enqueued. Missing
// required fields produce user-facing messages, not crashes.
func TestIntakeCreateGoal(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)

	reply := d.intakeCreateGoal(ctx, intakeAction{Goal: goalSub("从飞书建的任务", "", "a1", domID)})
	if !strings.Contains(reply, "已创建任务") {
		t.Fatalf("expected creation reply, got %q", reply)
	}
	var runStatus string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.status FROM run r JOIN goal g ON g.id=r.goal_id WHERE g.title=?`, "从飞书建的任务").Scan(&runStatus); err != nil {
		t.Fatalf("created goal must have a run: %v", err)
	}
	if runStatus != "queued" {
		t.Fatalf("first run must be queued for execution, got %q", runStatus)
	}

	// Missing title → the ask lists 标题 (the first call saved a goal draft
	// for "从飞书建的任务"; clear it so this case asks fresh).
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateGoal(ctx, intakeAction{Goal: goalSub("", "", "a1", domID)}); !strings.Contains(r, "还需要以下信息") || !strings.Contains(r, "标题") {
		t.Fatalf("missing title must ask with 标题, got %q", r)
	}
	// Hallucinated domain → service-layer validator message (all required
	// fields present, so it goes straight to Create).
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateGoal(ctx, intakeAction{Goal: goalSub("x", "", "a1", "nonexistent")}); !strings.Contains(r, "创建任务失败") {
		t.Fatalf("hallucinated domain must fail via the validator, got %q", r)
	}
	// Missing domain (title present) → the ask lists 项目/仓库 + the domain roster.
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateGoal(ctx, intakeAction{Goal: goalSub("修一下", "", "a1", "")}); !strings.Contains(r, "项目/仓库") {
		t.Fatalf("missing domain must ask with 项目/仓库, got %q", r)
	}
}

// TestIntakeReviewListAndStatus: the review queue and the status query are
// answered from the store, short ids accepted.
func TestIntakeReviewListAndStatus(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	g, err := d.goalSvc.Create(ctx, service.Goal{
		Title: "待审", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge: 必审' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}

	reply := d.intakeReviewList(ctx)
	if !strings.Contains(reply, "待审") || !strings.Contains(reply, "待审批") {
		t.Fatalf("review list must carry the pending goal: %q", reply)
	}

	status := d.intakeGoalStatus(ctx, g.ID[:8])
	if !strings.Contains(status, "review") || !strings.Contains(status, "待审") {
		t.Fatalf("status query must resolve the short id: %q", status)
	}
	if r := d.intakeGoalStatus(ctx, "zzzzzzzz"); !strings.Contains(r, "查询失败") {
		t.Fatalf("unknown id must fail cleanly: %q", r)
	}
}

// TestIntakeCreateSchedule: the platform executes the parsed schedule
// action through the service layer (cron validated, next_run computed);
// schedule_list and schedule_stop round-trip.
func TestIntakeCreateSchedule(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)

	reply := d.intakeCreateSchedule(ctx, intakeAction{Intent: "create_schedule", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "每小时巡检", Title: "定时巡检", Cron: "0 * * * *", AssigneeID: "a1", DomainID: domID}})
	if !strings.Contains(reply, "已创建定时任务") {
		t.Fatalf("expected creation reply, got %q", reply)
	}
	// Bad cron → the validator's message, not a crash.
	if r := d.intakeCreateSchedule(ctx, intakeAction{Intent: "create_schedule", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "坏cron", Title: "x", Cron: "not-a-cron", AssigneeID: "a1", DomainID: domID}}); !strings.Contains(r, "创建定时任务失败") {
		t.Fatalf("bad cron must surface the validator message, got %q", r)
	}

	list := d.intakeScheduleList(ctx)
	if !strings.Contains(list, "每小时巡检") || !strings.Contains(list, "0 * * * *") {
		t.Fatalf("schedule list must carry the created schedule: %q", list)
	}

	stop := d.intakeScheduleStop(ctx, intakeAction{Intent: "schedule_stop", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "每小时巡检"}})
	if !strings.Contains(stop, "已停用") {
		t.Fatalf("expected stop reply, got %q", stop)
	}
	if r := d.intakeScheduleStop(ctx, intakeAction{Intent: "schedule_stop", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "不存在"}}); !strings.Contains(r, "没找到") {
		t.Fatalf("stopping an unknown schedule must say so, got %q", r)
	}
	// Disabled schedules drop out of the list.
	if r := d.intakeScheduleList(ctx); strings.Contains(r, "每小时巡检") {
		t.Fatalf("disabled schedule must leave the list: %q", r)
	}
}

func firstID(t *testing.T, ctx context.Context, d *Daemon, q string) string {
	t.Helper()
	var id string
	if err := d.st.DB().QueryRowContext(ctx, q).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// agentSub builds an agentAction for test call sites (so each test does not
// re-declare the full struct literal).
func agentSub(Name, RuntimeID, Description, SystemPrompt string, Skills []string, SkillsSpecified bool) agentAction {
	return agentAction{
		Name: Name, RuntimeID: RuntimeID, Description: Description,
		SystemPrompt: SystemPrompt, Skills: Skills, SkillsSpecified: SkillsSpecified,
	}
}

// goalSub builds a goalAction for test call sites.
func goalSub(Title, Description, AssigneeID, DomainID string) goalAction {
	return goalAction{Title: Title, Description: Description, AssigneeID: AssigneeID, DomainID: DomainID}
}

// squadSub builds a squadAction for test call sites.
func squadSub(Name, LeaderID, Description, Instructions string, MemberIDs []string) squadAction {
	return squadAction{
		Name: Name, LeaderID: LeaderID, Description: Description,
		Instructions: Instructions, MemberIDs: MemberIDs,
	}
}

// domainSub builds a domainAction for test call sites.
func domainSub(Name, Type, GitURL string) domainAction {
	return domainAction{Name: Name, Type: Type, GitURL: GitURL}
}

// TestIntakeCreateAgent: the platform executes create_agent through the
// agent service — persona + runtime + optional skills. Skills clarification
// fires only when the library is non-empty AND the owner did not mention
// skills (skills_specified=false). The clarification draft completes the
// agent on the owner's next reply.
func TestIntakeCreateAgent(t *testing.T) {
	ctx := context.Background()

	// --- No skills on the platform: created directly, no clarification. ---
	d, st := newIntakeDaemon(t)
	reply := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"代码审查", "rt1", "审查 PR 代码质量", "你是代码审查员", nil, false)})
	if !strings.Contains(reply, "已创建 agent") {
		t.Fatalf("expected creation reply, got %q", reply)
	}
	var agentName, agentSkills string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT name, skills FROM agent WHERE name=?`, "代码审查").Scan(&agentName, &agentSkills); err != nil {
		t.Fatalf("created agent must exist: %v", err)
	}
	if agentSkills != "[]" {
		t.Fatalf("agent with no skills must store []: got %q", agentSkills)
	}

	// Each missing-field case saves a draft; the daemon is shared across
	// these cases, so clear the draft between them (or a later case would
	// merge onto the earlier case's draft instead of asking fresh).
	clearIntakeDraft := func() { _ = d.intakeSvc.ClearDraft(ctx) }

	// Missing name → the ask lists 名称 (no hard "缺少名称" anymore).
	clearIntakeDraft()
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "rt1", "x", "y", nil, true)}); !strings.Contains(r, "还需要以下信息") || !strings.Contains(r, "名称") {
		t.Fatalf("missing name must ask with 名称, got %q", r)
	}
	// Missing runtime_id → the ask lists 运行时 + the runtime roster.
	clearIntakeDraft()
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"无运行时", "", "x", "y", nil, true)}); !strings.Contains(r, "运行时") || !strings.Contains(r, "rt1") {
		t.Fatalf("missing runtime must ask with the runtime roster, got %q", r)
	}
	// Missing BOTH name and runtime → one ask listing both (not two round-trips).
	clearIntakeDraft()
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "", "", "", nil, true)}); !strings.Contains(r, "名称") || !strings.Contains(r, "运行时") {
		t.Fatalf("missing name+runtime must ask for both at once, got %q", r)
	}
	// Hallucinated runtime_id → service-layer validator message (all fields
	// present, so it goes straight to Create, which rejects the bad id).
	clearIntakeDraft()
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"坏runtime", "nope", "x", "y", nil, true)}); !strings.Contains(r, "创建 agent 失败") {
		t.Fatalf("hallucinated runtime must fail via the validator, got %q", r)
	}

	// Ask-at-most-once: after a draft is saved, a VAGUE reply that still
	// misses required fields must NOT re-ask — it merges, clears the draft,
	// and fails at the service layer (terminal error). No second ask.
	clearIntakeDraft()
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "", "", "", nil, true)}); !strings.Contains(r, "还需要以下信息") {
		t.Fatalf("setup: missing name+runtime must ask, got %q", r)
	}
	// Draft is now saved. Vague reply supplies nothing useful.
	vague := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "", "", "", nil, true)})
	if strings.Contains(vague, "还需要以下信息") {
		t.Fatalf("vague reply must NOT re-ask (ask-at-most-once), got %q", vague)
	}
	if !strings.Contains(vague, "创建 agent 失败") {
		t.Fatalf("vague reply must fail at the service layer, got %q", vague)
	}
	if _, ok := d.loadDraftOfKind(ctx, "agent"); ok {
		t.Fatal("draft must be cleared after the clarification turn (even on failure)")
	}

	// --- Skills on the platform + owner did not mention skills → clarify. ---
	d2, st2 := newIntakeDaemon(t)
	if _, err := st2.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk1','git-helper','git helper',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	reply = d2.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"带skill的agent", "rt1", "x", "y", nil, false)})
	if !strings.Contains(reply, "还需要以下信息") || !strings.Contains(reply, "skills") || !strings.Contains(reply, "git-helper") {
		t.Fatalf("must ask for skills (listed as missing) and show the skill, got %q", reply)
	}
	// The draft is saved — the clarification turn must build from it.
	if _, ok := d2.loadDraftOfKind(ctx, "agent"); !ok {
		t.Fatal("agent-kind draft must be saved for the clarification")
	}
	// Clarification turn: owner picks skills → agent created from the draft
	// (the draft carried name/runtime/description/system_prompt; the reply
	// supplies only skills).
	reply = d2.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "", "", "", []string{"sk1"}, true)})
	if !strings.Contains(reply, "已创建 agent") {
		t.Fatalf("clarification turn must create the agent, got %q", reply)
	}
	if _, ok := d2.loadDraftOfKind(ctx, "agent"); ok {
		t.Fatal("draft must be cleared after the agent is created")
	}
	var skillsJSON string
	if err := st2.DB().QueryRowContext(ctx,
		`SELECT skills FROM agent WHERE name=?`, "带skill的agent").Scan(&skillsJSON); err != nil {
		t.Fatalf("agent must exist: %v", err)
	}
	if !strings.Contains(skillsJSON, "sk1") {
		t.Fatalf("agent skills must carry sk1, got %q", skillsJSON)
	}

	// --- Skills on the platform + owner explicitly declined → no ask. ---
	d3, _ := newIntakeDaemon(t)
	if _, err := d3.st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk2','x','x',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	reply = d3.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"不要skill的agent", "rt1", "x", "y", nil, true)})
	if !strings.Contains(reply, "已创建 agent") {
		t.Fatalf("explicit decline must create directly, got %q", reply)
	}
}

// TestIntakeCreateSquad: the platform executes create_squad through the
// squad service (leader + optional members). Hallucinated leader/member ids
// surface as failures, not crashes.
func TestIntakeCreateSquad(t *testing.T) {
	ctx := context.Background()

	// Bare squad (leader only) — the minimal viable squad.
	d, st := newIntakeDaemon(t)
	reply := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"审查组", "a1", "PR 审查小组", "leader 拆分并委派子目标", nil)})
	if !strings.Contains(reply, "已创建 squad") {
		t.Fatalf("expected creation reply, got %q", reply)
	}
	var squadName string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT name FROM squad WHERE name=?`, "审查组").Scan(&squadName); err != nil {
		t.Fatalf("created squad must exist: %v", err)
	}

	// Missing name → the ask lists 名称 (clear the draft between cases —
	// each missing case saves a squad draft on the shared daemon).
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"", "a1", "x", "y", nil)}); !strings.Contains(r, "还需要以下信息") || !strings.Contains(r, "名称") {
		t.Fatalf("missing name must ask with 名称, got %q", r)
	}
	// Missing leader_id → the ask lists leader agent + the agent roster.
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"无leader", "", "x", "y", nil)}); !strings.Contains(r, "leader agent") || !strings.Contains(r, "worker1") {
		t.Fatalf("missing leader must ask with the agent roster, got %q", r)
	}
	// Missing BOTH name and leader → one ask listing both.
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"", "", "x", "y", nil)}); !strings.Contains(r, "名称") || !strings.Contains(r, "leader agent") {
		t.Fatalf("missing name+leader must ask for both at once, got %q", r)
	}
	// Hallucinated leader_id → service-layer validator message.
	_ = d.intakeSvc.ClearDraft(ctx)
	if r := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"坏leader", "nope", "x", "y", nil)}); !strings.Contains(r, "创建 squad 失败") {
		t.Fatalf("hallucinated leader must fail via the validator, got %q", r)
	}

	// Squad with a member — the member row is attached after Create.
	d2, st2 := newIntakeDaemon(t)
	if _, err := st2.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	reply = d2.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"带成员的组", "a1", "x", "y", []string{"a2"})})
	if !strings.Contains(reply, "已创建 squad") {
		t.Fatalf("squad with member must create, got %q", reply)
	}
	var memberCount int
	if err := st2.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad_member WHERE squad_id=(SELECT id FROM squad WHERE name=?)`, "带成员的组").Scan(&memberCount); err != nil {
		t.Fatalf("query members: %v", err)
	}
	if memberCount != 1 {
		t.Fatalf("squad must have 1 member, got %d", memberCount)
	}

	// Hallucinated member → partial success (squad created, member failed).
	d3, _ := newIntakeDaemon(t)
	reply = d3.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"部分成功", "a1", "x", "y", []string{"ghost"})})
	if !strings.Contains(reply, "已创建 squad") || !strings.Contains(reply, "添加失败") {
		t.Fatalf("hallucinated member must be partial success, got %q", reply)
	}

	// The leader id in member_ids is skipped (it is already squad.leader_id).
	d4, st4 := newIntakeDaemon(t)
	reply = d4.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"leader重复", "a1", "x", "y", []string{"a1"})})
	if !strings.Contains(reply, "已创建 squad") {
		t.Fatalf("leader-as-member must still create, got %q", reply)
	}
	if err := st4.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad_member WHERE squad_id=(SELECT id FROM squad WHERE name=?)`, "leader重复").Scan(&memberCount); err != nil {
		t.Fatalf("query members: %v", err)
	}
	if memberCount != 0 {
		t.Fatalf("leader must not be attached as a member, got %d", memberCount)
	}
}

// TestIntakeRegistryIntents documents the fixed IM-intent set. With the
// switch eliminated (dispatch derives from this registry), there is no
// parallel structure to drift from — this test is now pure documentation
// of the expected surface, not a sync guard.
func TestIntakeRegistryIntents(t *testing.T) {
	want := map[string]bool{
		"create_goal": true, "goal_list": true, "goal_cancel": true, "goal_assign": true,
		"goal_reopen": true, "goal_delete": true,
		"review_list": true, "goal_status": true,
		"create_schedule": true, "schedule_list": true, "schedule_stop": true,
		"schedule_enable": true, "schedule_delete": true,
		"create_agent": true, "agent_list": true, "agent_delete": true, "agent_update": true,
		"create_squad": true,
		"squad_list":   true, "squad_detail": true, "squad_update": true,
		"squad_add_member": true, "squad_remove_member": true, "squad_delete": true,
		"import_team":   true,
		"domain_create": true, "domain_list": true, "domain_delete": true,
		"skill_list": true, "skill_delete": true,
	}
	got := make(map[string]bool)
	for _, c := range intakeReg.cmds {
		got[c.intent] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("intakeReg missing intent %q", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("intakeReg has %d entries, want %d", len(got), len(want))
	}
}

// TestIntakeDispatchUnknownReturnsFallback verifies the not-found path of
// dispatch — an intent absent from the registry yields the fallback prompt,
// not a panic or empty string.
func TestIntakeDispatchUnknownReturnsFallback(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	reply := intakeReg.dispatch(d, ctx, intakeAction{Intent: "totally_bogus"})
	if !strings.Contains(reply, "没听懂") {
		t.Fatalf("unknown intent must return fallback, got %q", reply)
	}
}

// TestIntakeSquadListDetail: list shows all squads; detail shows the roster.
func TestIntakeSquadListDetail(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()

	// Empty list.
	if r := d.intakeSquadList(ctx); !strings.Contains(r, "没有 squad") {
		t.Fatalf("empty list, got %q", r)
	}

	// Create a squad with a member.
	d2, st2 := newIntakeDaemon(t)
	if _, err := st2.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	d2.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"审查组", "a1", "PR 审查", "leader 拆分委派", []string{"a2"})})

	list := d2.intakeSquadList(ctx)
	if !strings.Contains(list, "审查组") || !strings.Contains(list, "worker1") {
		t.Fatalf("list must show squad name + leader name, got %q", list)
	}
	if !strings.Contains(list, "成员 1") {
		t.Fatalf("list must show member count, got %q", list)
	}

	detail := d2.intakeSquadDetail(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", nil)})
	if !strings.Contains(detail, "审查组") || !strings.Contains(detail, "worker1") || !strings.Contains(detail, "worker2") {
		t.Fatalf("detail must show squad + leader + member, got %q", detail)
	}
	if !strings.Contains(detail, "leader 拆分委派") {
		t.Fatalf("detail must show instructions, got %q", detail)
	}

	// Detail on non-existent squad.
	if r := d2.intakeSquadDetail(ctx, intakeAction{Squad: squadSub("不存在", "", "", "", nil)}); !strings.Contains(r, "没找到") {
		t.Fatalf("detail on missing squad must say so, got %q", r)
	}

	// Detail without name.
	if r := d2.intakeSquadDetail(ctx, intakeAction{}); !strings.Contains(r, "需要名字") {
		t.Fatalf("detail without name must ask, got %q", r)
	}
}

// TestIntakeSquadUpdate: partial update changes only the specified field.
func TestIntakeSquadUpdate(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"审查组", "a1", "PR 审查", "leader 拆分委派", nil)})

	// Change leader a1 → a2.
	reply := d.intakeSquadUpdate(ctx, intakeAction{Squad: squadSub("审查组", "a2", "", "", nil)})
	if !strings.Contains(reply, "已更新") {
		t.Fatalf("update leader must succeed, got %q", reply)
	}
	var leaderID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT leader_id FROM squad WHERE name=?`, "审查组").Scan(&leaderID); err != nil {
		t.Fatal(err)
	}
	if leaderID != "a2" {
		t.Fatalf("leader must be a2 after update, got %q", leaderID)
	}
	// No fields specified → ask what to change.
	if r := d.intakeSquadUpdate(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", nil)}); !strings.Contains(r, "需要指定") {
		t.Fatalf("update with no changes must ask, got %q", r)
	}
	// Non-existent squad.
	if r := d.intakeSquadUpdate(ctx, intakeAction{Squad: squadSub("不存在", "a1", "", "", nil)}); !strings.Contains(r, "没找到") {
		t.Fatalf("update on missing squad must say so, got %q", r)
	}
}

// TestIntakeSquadAddRemoveMember: add then remove a member from an existing squad.
func TestIntakeSquadAddRemoveMember(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"审查组", "a1", "PR 审查", "拆分委派", nil)})

	// Add a2.
	reply := d.intakeSquadAddMember(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", []string{"a2"})})
	if !strings.Contains(reply, "已添加") {
		t.Fatalf("add member must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad_member WHERE squad_id=(SELECT id FROM squad WHERE name=?)`, "审查组").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("squad must have 1 member after add, got %d", count)
	}

	// Remove a2.
	reply = d.intakeSquadRemoveMember(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", []string{"a2"})})
	if !strings.Contains(reply, "已从") || !strings.Contains(reply, "移除") {
		t.Fatalf("remove member must succeed, got %q", reply)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM squad_member WHERE squad_id=(SELECT id FROM squad WHERE name=?)`, "审查组").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("squad must have 0 members after remove, got %d", count)
	}

	// Remove a member not in the squad.
	if r := d.intakeSquadRemoveMember(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", []string{"a2"})}); !strings.Contains(r, "不在") {
		t.Fatalf("removing a non-member must report it, got %q", r)
	}

	// Add without specifying member.
	if r := d.intakeSquadAddMember(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", nil)}); !strings.Contains(r, "需要指定") {
		t.Fatalf("add without member must ask, got %q", r)
	}
}

// TestIntakeSquadDelete: delete removes the squad.
func TestIntakeSquadDelete(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"审查组", "a1", "PR 审查", "拆分委派", nil)})

	reply := d.intakeSquadDelete(ctx, intakeAction{Squad: squadSub("审查组", "", "", "", nil)})
	if !strings.Contains(reply, "已删除") {
		t.Fatalf("delete must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM squad WHERE name=?`, "审查组").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("squad must be deleted, got %d rows", count)
	}

	// Delete non-existent.
	if r := d.intakeSquadDelete(ctx, intakeAction{Squad: squadSub("不存在", "", "", "", nil)}); !strings.Contains(r, "没找到") {
		t.Fatalf("delete on missing squad must say so, got %q", r)
	}
}

// TestIntakeSquadDeleteBatch: comma-separated names delete individually with
// per-item result + summary.
func TestIntakeSquadDeleteBatch(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub("组A", "a1", "", "", nil)})
	d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub("组B", "a1", "", "", nil)})

	reply := d.intakeSquadDelete(ctx, intakeAction{Squad: squadSub("组A, 组B, 不存在", "", "", "", nil)})
	if !strings.Contains(reply, "组A") || !strings.Contains(reply, "组B") {
		t.Fatalf("must report deleted squads, got %q", reply)
	}
	if !strings.Contains(reply, "不存在") {
		t.Fatalf("must report failed squad, got %q", reply)
	}
	if !strings.Contains(reply, "成功 2") || !strings.Contains(reply, "失败 1") {
		t.Fatalf("must have summary, got %q", reply)
	}
}

// TestIntakeListGoals: empty list says so; populated list carries title,
// short id, status, and assignee display name.
func TestIntakeListGoals(t *testing.T) {
	ctx := context.Background()

	// Empty.
	d, _ := newIntakeDaemon(t)
	if r := d.intakeListGoals(ctx); !strings.Contains(r, "没有任务") {
		t.Fatalf("empty list, got %q", r)
	}

	// With goals.
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	d.intakeCreateGoal(ctx, intakeAction{Goal: goalSub("任务A", "", "a1", domID)})
	d.intakeCreateGoal(ctx, intakeAction{Goal: goalSub("任务B", "", "a1", domID)})
	list := d.intakeListGoals(ctx)
	if !strings.Contains(list, "任务A") || !strings.Contains(list, "任务B") {
		t.Fatalf("list must show both goals, got %q", list)
	}
	if !strings.Contains(list, "worker1") {
		t.Fatalf("list must show assignee name, got %q", list)
	}
}

// TestIntakeCancelGoal: cancel succeeds on an active goal; re-canceling a
// cancelled goal fails; missing id asks for one; unknown id says not found.
func TestIntakeCancelGoal(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	g, err := d.goalSvc.Create(ctx, service.Goal{
		Title: "可取消", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}

	reply := d.intakeCancelGoal(ctx, g.ID[:8])
	if !strings.Contains(reply, "已取消") {
		t.Fatalf("cancel must succeed, got %q", reply)
	}

	// Re-cancel → fails (goal is terminal).
	if r := d.intakeCancelGoal(ctx, g.ID[:8]); !strings.Contains(r, "取消失败") {
		t.Fatalf("re-cancel must fail, got %q", r)
	}
	// No id.
	if r := d.intakeCancelGoal(ctx, ""); !strings.Contains(r, "需要任务 id") {
		t.Fatalf("no id must ask, got %q", r)
	}
	// Unknown id.
	if r := d.intakeCancelGoal(ctx, "zzzzzzzz"); !strings.Contains(r, "找不到") {
		t.Fatalf("unknown id must say not found, got %q", r)
	}
}

// TestIntakeAssignGoal: assign succeeds and changes the assignee; missing
// assignee asks; hallucinated agent fails at the service layer.
func TestIntakeAssignGoal(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	g, err := d.goalSvc.Create(ctx, service.Goal{
		Title: "可转交", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}

	reply := d.intakeAssignGoal(ctx, intakeAction{
		GoalID: g.ID[:8], Goal: goalAction{AssigneeID: "a2", AssigneeType: "agent"}})
	if !strings.Contains(reply, "已转交") || !strings.Contains(reply, "worker2") {
		t.Fatalf("assign must succeed with new assignee name, got %q", reply)
	}
	var assigneeID string
	if err := st.DB().QueryRowContext(ctx, `SELECT assignee_id FROM goal WHERE id=?`, g.ID).Scan(&assigneeID); err != nil {
		t.Fatal(err)
	}
	if assigneeID != "a2" {
		t.Fatalf("assignee must be a2, got %q", assigneeID)
	}

	// Missing assignee.
	if r := d.intakeAssignGoal(ctx, intakeAction{GoalID: g.ID[:8]}); !strings.Contains(r, "需要指定执行者") {
		t.Fatalf("missing assignee must ask, got %q", r)
	}
	// Missing goal id.
	if r := d.intakeAssignGoal(ctx, intakeAction{}); !strings.Contains(r, "需要任务 id") {
		t.Fatalf("missing id must ask, got %q", r)
	}
	// Hallucinated agent.
	if r := d.intakeAssignGoal(ctx, intakeAction{
		GoalID: g.ID[:8], Goal: goalAction{AssigneeID: "ghost", AssigneeType: "agent"}}); !strings.Contains(r, "转交失败") {
		t.Fatalf("hallucinated agent must fail, got %q", r)
	}
}

// TestIntakeListAgents: empty list says so; populated list carries name and
// description.
func TestIntakeListAgents(t *testing.T) {
	ctx := context.Background()

	// The test daemon seeds a1/worker1, so list is non-empty by default.
	d, _ := newIntakeDaemon(t)
	list := d.intakeListAgents(ctx)
	if !strings.Contains(list, "worker1") {
		t.Fatalf("list must show seeded agent, got %q", list)
	}
}

// TestIntakeDeleteAgent: delete succeeds on a free agent; an agent with a
// goal is guarded; non-existent name says not found.
func TestIntakeDeleteAgent(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	reply := d.intakeDeleteAgent(ctx, intakeAction{Agent: agentSub("worker2", "rt1", "", "", nil, true)})
	if !strings.Contains(reply, "已删除") {
		t.Fatalf("delete must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE name=?`, "worker2").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("agent must be deleted, got %d rows", count)
	}

	// Agent with a goal → guarded.
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	d.goalSvc.Create(ctx, service.Goal{
		Title: "占用a1", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if r := d.intakeDeleteAgent(ctx, intakeAction{Agent: agentSub("worker1", "rt1", "", "", nil, true)}); !strings.Contains(r, "失败") {
		t.Fatalf("agent with goal must fail, got %q", r)
	}

	// Non-existent.
	if r := d.intakeDeleteAgent(ctx, intakeAction{Agent: agentSub("不存在", "rt1", "", "", nil, true)}); !strings.Contains(r, "没找到") {
		t.Fatalf("non-existent must say so, got %q", r)
	}
}

// TestIntakeDeleteAgentBatch: comma-separated names delete individually with
// per-item result; partial success + guarded failure + summary line.
func TestIntakeDeleteAgentBatch(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	// Seed worker2, worker3 (worker1 is seeded by newIntakeDaemon).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a2','worker2','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,runtime_id,max_concurrent,created_at) VALUES ('a3','worker3','rt1',1,?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// worker1 has a goal → guarded.
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	d.goalSvc.Create(ctx, service.Goal{
		Title: "占用a1", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})

	// Batch: worker2 ✅, worker1 ❌ (guarded), worker3 ✅, ghost ❌ (not found).
	reply := d.intakeDeleteAgent(ctx, intakeAction{Agent: agentSub("worker2, worker1, worker3, ghost", "", "", "", nil, true)})
	if !strings.Contains(reply, "worker2") || !strings.Contains(reply, "worker3") {
		t.Fatalf("must report deleted agents, got %q", reply)
	}
	if !strings.Contains(reply, "worker1") || !strings.Contains(reply, "ghost") {
		t.Fatalf("must report failed agents, got %q", reply)
	}
	if !strings.Contains(reply, "成功 2") || !strings.Contains(reply, "失败 2") {
		t.Fatalf("must have summary line, got %q", reply)
	}
	// worker2 and worker3 gone, worker1 still exists.
	for _, name := range []string{"worker2", "worker3"} {
		var n int
		if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE name=?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s must be deleted", name)
		}
	}
}

// TestIntakeUpdateAgent: partial update changes only the specified field;
// no changes asks what to change; non-existent name says not found.
func TestIntakeUpdateAgent(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()

	// Change description.
	reply := d.intakeUpdateAgent(ctx, intakeAction{Agent: agentSub("worker1", "", "新描述", "", nil, true)})
	if !strings.Contains(reply, "已更新") {
		t.Fatalf("update must succeed, got %q", reply)
	}
	var desc string
	if err := st.DB().QueryRowContext(ctx, `SELECT description FROM agent WHERE name=?`, "worker1").Scan(&desc); err != nil {
		t.Fatal(err)
	}
	if desc != "新描述" {
		t.Fatalf("description must change, got %q", desc)
	}

	// Change system_prompt.
	reply = d.intakeUpdateAgent(ctx, intakeAction{Agent: agentSub("worker1", "", "", "新人设", nil, true)})
	if !strings.Contains(reply, "已更新") {
		t.Fatalf("update system_prompt must succeed, got %q", reply)
	}
	var sp string
	if err := st.DB().QueryRowContext(ctx, `SELECT system_prompt FROM agent WHERE name=?`, "worker1").Scan(&sp); err != nil {
		t.Fatal(err)
	}
	if sp != "新人设" {
		t.Fatalf("system_prompt must change, got %q", sp)
	}

	// No fields specified.
	if r := d.intakeUpdateAgent(ctx, intakeAction{Agent: agentSub("worker1", "", "", "", nil, true)}); !strings.Contains(r, "需要指定") {
		t.Fatalf("no changes must ask, got %q", r)
	}

	// Non-existent.
	if r := d.intakeUpdateAgent(ctx, intakeAction{Agent: agentSub("不存在", "", "x", "", nil, true)}); !strings.Contains(r, "没找到") {
		t.Fatalf("non-existent must say so, got %q", r)
	}
}

// TestIntakeCreateDomain: repo domain creates; scratch domain creates without
// git_url; missing name asks; missing git_url asks; draft clarification
// completes in two steps.
func TestIntakeCreateDomain(t *testing.T) {
	ctx := context.Background()

	// Repo domain — full fields.
	d, st := newIntakeDaemon(t)
	reply := d.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("myrepo", "repo", "https://e.com/myrepo.git")})
	if !strings.Contains(reply, "已创建项目") {
		t.Fatalf("repo domain must create, got %q", reply)
	}
	var name string
	if err := st.DB().QueryRowContext(ctx, `SELECT name FROM domain WHERE name=?`, "myrepo").Scan(&name); err != nil {
		t.Fatalf("domain must exist: %v", err)
	}

	// Scratch domain — no git_url needed.
	d2, st2 := newIntakeDaemon(t)
	reply = d2.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("myscratch", "scratch", "")})
	if !strings.Contains(reply, "已创建项目") {
		t.Fatalf("scratch domain must create without git_url, got %q", reply)
	}
	var dtype string
	if err := st2.DB().QueryRowContext(ctx, `SELECT type FROM domain WHERE name=?`, "myscratch").Scan(&dtype); err != nil {
		t.Fatalf("scratch domain must exist: %v", err)
	}
	if dtype != "scratch" {
		t.Fatalf("type must be scratch, got %q", dtype)
	}

	// Missing name → ask.
	d3, _ := newIntakeDaemon(t)
	_ = d3.intakeSvc.ClearDraft(ctx)
	if r := d3.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("", "repo", "https://e.com/x.git")}); !strings.Contains(r, "还需要") || !strings.Contains(r, "名称") {
		t.Fatalf("missing name must ask, got %q", r)
	}

	// Missing git_url (repo) → ask.
	d4, _ := newIntakeDaemon(t)
	_ = d4.intakeSvc.ClearDraft(ctx)
	if r := d4.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("no-url", "repo", "")}); !strings.Contains(r, "还需要") || !strings.Contains(r, "仓库地址") {
		t.Fatalf("missing git_url must ask, got %q", r)
	}

	// Draft clarification: first message has name but no git_url → ask;
	// second message supplies git_url → create from draft.
	d5, _ := newIntakeDaemon(t)
	_ = d5.intakeSvc.ClearDraft(ctx)
	ask := d5.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("draftrepo", "repo", "")})
	if !strings.Contains(ask, "仓库地址") {
		t.Fatalf("draft setup must ask for git_url, got %q", ask)
	}
	// Clarification turn: supply git_url (name carried by draft).
	reply = d5.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("", "", "https://e.com/draft.git")})
	if !strings.Contains(reply, "已创建项目") {
		t.Fatalf("draft clarification must create, got %q", reply)
	}
}

// TestIntakeListDomains: the test daemon seeds d1, so list is non-empty.
func TestIntakeListDomains(t *testing.T) {
	ctx := context.Background()
	d, _ := newIntakeDaemon(t)
	list := d.intakeListDomains(ctx)
	if !strings.Contains(list, "d1") {
		t.Fatalf("list must show seeded domain d1, got %q", list)
	}
	if !strings.Contains(list, "repo") {
		t.Fatalf("list must show domain type, got %q", list)
	}
}

// TestIntakeReopenGoal: reopen succeeds on a cancelled goal; reopening an
// active goal fails; missing id asks; unknown id says not found.
func TestIntakeReopenGoal(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	g, err := d.goalSvc.Create(ctx, service.Goal{
		Title: "可重开", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	// Cancel it first so it becomes reopenable.
	if _, err := d.goalSvc.Cancel(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	reply := d.intakeReopenGoal(ctx, intakeAction{GoalID: g.ID[:8]})
	if !strings.Contains(reply, "已重开") {
		t.Fatalf("reopen must succeed, got %q", reply)
	}
	// Reopen an active goal → fails.
	d.goalSvc.Create(ctx, service.Goal{
		Title: "活跃的", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	var activeID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT id FROM goal WHERE title=?`, "活跃的").Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if r := d.intakeReopenGoal(ctx, intakeAction{GoalID: activeID[:8]}); !strings.Contains(r, "重开失败") {
		t.Fatalf("reopen active must fail, got %q", r)
	}
	// No id.
	if r := d.intakeReopenGoal(ctx, intakeAction{}); !strings.Contains(r, "需要任务 id") {
		t.Fatalf("no id must ask, got %q", r)
	}
	// Unknown id.
	if r := d.intakeReopenGoal(ctx, intakeAction{GoalID: "zzzzzzzz"}); !strings.Contains(r, "找不到") {
		t.Fatalf("unknown id must say not found, got %q", r)
	}
}

// TestIntakeDeleteGoal: delete succeeds on a free goal; missing id asks;
// unknown id says not found.
func TestIntakeDeleteGoal(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	g, err := d.goalSvc.Create(ctx, service.Goal{
		Title: "可删除", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	reply := d.intakeDeleteGoal(ctx, g.ID[:8])
	if !strings.Contains(reply, "已删除") {
		t.Fatalf("delete must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal WHERE id=?`, g.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("goal must be deleted, got %d rows", count)
	}
	// No id.
	if r := d.intakeDeleteGoal(ctx, ""); !strings.Contains(r, "需要任务 id") {
		t.Fatalf("no id must ask, got %q", r)
	}
	// Unknown id.
	if r := d.intakeDeleteGoal(ctx, "zzzzzzzz"); !strings.Contains(r, "找不到") {
		t.Fatalf("unknown id must say not found, got %q", r)
	}
}

// TestIntakeScheduleEnable: enable succeeds on a disabled schedule;
// enabling an already-enabled schedule says not found (among disabled);
// recompute next_run_at puts it in the future.
func TestIntakeScheduleEnable(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	// Create + stop a schedule.
	d.intakeCreateSchedule(ctx, intakeAction{Intent: "create_schedule", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "每小时巡检", Title: "定时巡检", Cron: "0 * * * *", AssigneeID: "a1", DomainID: domID}})
	d.intakeScheduleStop(ctx, intakeAction{Intent: "schedule_stop", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "每小时巡检"}})

	// Enable it back.
	reply := d.intakeScheduleEnable(ctx, intakeAction{Intent: "schedule_enable", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "每小时巡检"}})
	if !strings.Contains(reply, "已启用") {
		t.Fatalf("enable must succeed, got %q", reply)
	}
	// next_run_at must be in the future.
	var nextStr string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT next_run_at FROM schedule WHERE name=?`, "每小时巡检").Scan(&nextStr); err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, nextStr)
	if err != nil {
		t.Fatalf("next_run_at must be parseable: %v", err)
	}
	if !next.After(time.Now()) {
		t.Fatalf("next_run_at must be in the future, got %v", next)
	}
	// Enable non-existent.
	if r := d.intakeScheduleEnable(ctx, intakeAction{Intent: "schedule_enable", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "不存在"}}); !strings.Contains(r, "没找到") {
		t.Fatalf("enable non-existent must say so, got %q", r)
	}
}

// TestIntakeScheduleDelete: delete removes the schedule (enabled or disabled);
// non-existent says not found.
func TestIntakeScheduleDelete(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	domID := firstID(t, ctx, d, `SELECT id FROM domain`)
	d.intakeCreateSchedule(ctx, intakeAction{Intent: "create_schedule", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "待删除", Title: "x", Cron: "0 * * * *", AssigneeID: "a1", DomainID: domID}})

	reply := d.intakeScheduleDelete(ctx, intakeAction{Intent: "schedule_delete", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "待删除"}})
	if !strings.Contains(reply, "已删除") {
		t.Fatalf("delete must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schedule WHERE name=?`, "待删除").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("schedule must be deleted, got %d rows", count)
	}
	// Non-existent.
	if r := d.intakeScheduleDelete(ctx, intakeAction{Intent: "schedule_delete", Schedule: struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"`
		DomainID     string `json:"domain_id"`
	}{Name: "不存在"}}); !strings.Contains(r, "没找到") {
		t.Fatalf("delete non-existent must say so, got %q", r)
	}
}

// TestIntakeListSkills: empty list says so; populated list carries name.
func TestIntakeListSkills(t *testing.T) {
	ctx := context.Background()

	// Empty.
	d, _ := newIntakeDaemon(t)
	if r := d.intakeListSkills(ctx); !strings.Contains(r, "没有 skill") {
		t.Fatalf("empty list, got %q", r)
	}
	// With a skill.
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk1','git-helper','git helper',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	list := d.intakeListSkills(ctx)
	if !strings.Contains(list, "git-helper") {
		t.Fatalf("list must show skill name, got %q", list)
	}
}

// TestIntakeDeleteSkill: delete succeeds on a free skill; skill selected by
// an agent is guarded; non-existent says not found.
func TestIntakeDeleteSkill(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk1','git-helper','git helper',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	reply := d.intakeDeleteSkill(ctx, intakeAction{Intent: "skill_delete", Skill: skillAction{Name: "git-helper"}})
	if !strings.Contains(reply, "已删除") {
		t.Fatalf("delete must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM skill WHERE name=?`, "git-helper").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("skill must be deleted, got %d rows", count)
	}
	// Skill selected by an agent → guarded.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES ('sk2','web-scraper','scrapes web',?)`,
		time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Agent a1 selects sk2 (the skills column stores JSON array of id strings).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE agent SET skills='["sk2"]' WHERE id='a1'`); err != nil {
		t.Fatal(err)
	}
	if r := d.intakeDeleteSkill(ctx, intakeAction{Intent: "skill_delete", Skill: skillAction{Name: "web-scraper"}}); !strings.Contains(r, "失败") {
		t.Fatalf("skill selected by agent must fail, got %q", r)
	}
	// Non-existent.
	if r := d.intakeDeleteSkill(ctx, intakeAction{Intent: "skill_delete", Skill: skillAction{Name: "不存在"}}); !strings.Contains(r, "没找到") {
		t.Fatalf("non-existent must say so, got %q", r)
	}
}

// TestIntakeDeleteSkillBatch: comma-separated names delete individually.
func TestIntakeDeleteSkillBatch(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	now := time.Now().Format(time.RFC3339Nano)
	for _, s := range []struct{ id, name string }{
		{"sk1", "skill-a"}, {"sk2", "skill-b"}, {"sk3", "skill-c"},
	} {
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO skill (id,name,description,created_at) VALUES (?,?,?,?)`, s.id, s.name, "", now); err != nil {
			t.Fatal(err)
		}
	}
	// sk3 is selected by agent a1 → guarded.
	if _, err := st.DB().ExecContext(ctx, `UPDATE agent SET skills='["sk3"]' WHERE id='a1'`); err != nil {
		t.Fatal(err)
	}

	reply := d.intakeDeleteSkill(ctx, intakeAction{Intent: "skill_delete", Skill: skillAction{Name: "skill-a, skill-c, skill-b, ghost"}})
	if !strings.Contains(reply, "skill-a") || !strings.Contains(reply, "skill-b") {
		t.Fatalf("must report deleted skills, got %q", reply)
	}
	if !strings.Contains(reply, "skill-c") || !strings.Contains(reply, "ghost") {
		t.Fatalf("must report failed skills, got %q", reply)
	}
	if !strings.Contains(reply, "成功 2") || !strings.Contains(reply, "失败 2") {
		t.Fatalf("must have summary, got %q", reply)
	}
}

// TestIntakeDeleteDomain: delete succeeds on a free domain; domain with a
// goal is guarded; non-existent says not found.
func TestIntakeDeleteDomain(t *testing.T) {
	d, st := newIntakeDaemon(t)
	ctx := context.Background()
	// Create an extra domain (d1 is seeded and has a goal in some tests).
	d.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("free-repo", "repo", "https://e.com/free.git")})

	reply := d.intakeDeleteDomain(ctx, intakeAction{Intent: "domain_delete", Domain: domainSub("free-repo", "", "")})
	if !strings.Contains(reply, "已删除") {
		t.Fatalf("delete must succeed, got %q", reply)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM domain WHERE name=?`, "free-repo").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("domain must be deleted, got %d rows", count)
	}
	// Domain with a goal → guarded (d1 has no goal yet, create one).
	domID := firstID(t, ctx, d, `SELECT id FROM domain WHERE name='d1'`)
	d.goalSvc.Create(ctx, service.Goal{
		Title: "占用d1", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})
	if r := d.intakeDeleteDomain(ctx, intakeAction{Intent: "domain_delete", Domain: domainSub("d1", "", "")}); !strings.Contains(r, "失败") {
		t.Fatalf("domain with goal must fail, got %q", r)
	}
	// Non-existent.
	if r := d.intakeDeleteDomain(ctx, intakeAction{Intent: "domain_delete", Domain: domainSub("不存在", "", "")}); !strings.Contains(r, "没找到") {
		t.Fatalf("non-existent must say so, got %q", r)
	}
}

// TestIntakeDeleteDomainBatch: comma-separated names delete individually.
func TestIntakeDeleteDomainBatch(t *testing.T) {
	d, _ := newIntakeDaemon(t)
	ctx := context.Background()
	d.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("repo-a", "repo", "https://e.com/a.git")})
	d.intakeCreateDomain(ctx, intakeAction{Intent: "domain_create", Domain: domainSub("repo-b", "repo", "https://e.com/b.git")})
	// d1 has a goal → guarded.
	domID := firstID(t, ctx, d, `SELECT id FROM domain WHERE name='d1'`)
	d.goalSvc.Create(ctx, service.Goal{
		Title: "占用d1", DomainID: domID, AssigneeType: "agent", AssigneeID: "a1", Status: "active"})

	reply := d.intakeDeleteDomain(ctx, intakeAction{Intent: "domain_delete", Domain: domainSub("repo-a, d1, repo-b, ghost", "", "")})
	if !strings.Contains(reply, "repo-a") || !strings.Contains(reply, "repo-b") {
		t.Fatalf("must report deleted domains, got %q", reply)
	}
	if !strings.Contains(reply, "d1") || !strings.Contains(reply, "ghost") {
		t.Fatalf("must report failed domains, got %q", reply)
	}
	if !strings.Contains(reply, "成功 2") || !strings.Contains(reply, "失败 2") {
		t.Fatalf("must have summary, got %q", reply)
	}
}
