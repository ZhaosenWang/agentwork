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
	d := &Daemon{
		st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc, schedSvc: schedSvc,
		agentSvc: agentSvc, squadSvc: squadSvc, qs: notify.NewSQLQueryStore(st),
		intakeSvc: intakeSvc,
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
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Cron        string `json:"cron"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	}{Name: "每小时巡检", Title: "定时巡检", Cron: "0 * * * *", AssigneeID: "a1", DomainID: domID}})
	if !strings.Contains(reply, "已创建定时任务") {
		t.Fatalf("expected creation reply, got %q", reply)
	}
	// Bad cron → the validator's message, not a crash.
	if r := d.intakeCreateSchedule(ctx, intakeAction{Intent: "create_schedule", Schedule: struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Cron        string `json:"cron"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	}{Name: "坏cron", Title: "x", Cron: "not-a-cron", AssigneeID: "a1", DomainID: domID}}); !strings.Contains(r, "创建定时任务失败") {
		t.Fatalf("bad cron must surface the validator message, got %q", r)
	}

	list := d.intakeScheduleList(ctx)
	if !strings.Contains(list, "每小时巡检") || !strings.Contains(list, "0 * * * *") {
		t.Fatalf("schedule list must carry the created schedule: %q", list)
	}

	stop := d.intakeScheduleStop(ctx, intakeAction{Intent: "schedule_stop", Schedule: struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Cron        string `json:"cron"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	}{Name: "每小时巡检"}})
	if !strings.Contains(stop, "已停用") {
		t.Fatalf("expected stop reply, got %q", stop)
	}
	if r := d.intakeScheduleStop(ctx, intakeAction{Intent: "schedule_stop", Schedule: struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Cron        string `json:"cron"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
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
		"create_goal": true, "review_list": true, "goal_status": true,
		"create_schedule": true, "schedule_list": true, "schedule_stop": true,
		"create_agent": true, "create_squad": true,
		"squad_list": true, "squad_detail": true, "squad_update": true,
		"squad_add_member": true, "squad_remove_member": true, "squad_delete": true,
		"import_team": true,
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
