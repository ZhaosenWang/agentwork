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

	reply := d.intakeCreateGoal(ctx, intakeAction{Goal: struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	}{Title: "从飞书建的任务", DomainID: domID, AssigneeID: "a1"}})
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

	if r := d.intakeCreateGoal(ctx, intakeAction{Goal: struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	}{Title: "", DomainID: domID, AssigneeID: "a1"}}); !strings.Contains(r, "缺少标题") {
		t.Fatalf("missing title must surface, got %q", r)
	}
	if r := d.intakeCreateGoal(ctx, intakeAction{Goal: struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	}{Title: "x", DomainID: "nonexistent", AssigneeID: "a1"}}); !strings.Contains(r, "创建任务失败") {
		t.Fatalf("hallucinated domain must fail via the validator, got %q", r)
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

// agentSub is the anonymous-struct shape of intakeAction.Agent (kept here so
// each test call site does not re-declare the 6-field struct literal).
func agentSub(Name, RuntimeID, Description, SystemPrompt string, Skills []string, SkillsSpecified bool) struct {
	Name            string   `json:"name"`
	RuntimeID       string   `json:"runtime_id"`
	Description     string   `json:"description"`
	SystemPrompt    string   `json:"system_prompt"`
	Skills          []string `json:"skills"`
	SkillsSpecified bool     `json:"skills_specified"`
} {
	return struct {
		Name            string   `json:"name"`
		RuntimeID       string   `json:"runtime_id"`
		Description     string   `json:"description"`
		SystemPrompt    string   `json:"system_prompt"`
		Skills          []string `json:"skills"`
		SkillsSpecified bool     `json:"skills_specified"`
	}{Name, RuntimeID, Description, SystemPrompt, Skills, SkillsSpecified}
}

// squadSub is the anonymous-struct shape of intakeAction.Squad.
func squadSub(Name, LeaderID, Description, Instructions string, MemberIDs []string) struct {
	Name         string   `json:"name"`
	LeaderID     string   `json:"leader_id"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	MemberIDs    []string `json:"member_ids"`
} {
	return struct {
		Name         string   `json:"name"`
		LeaderID     string   `json:"leader_id"`
		Description  string   `json:"description"`
		Instructions string   `json:"instructions"`
		MemberIDs    []string `json:"member_ids"`
	}{Name, LeaderID, Description, Instructions, MemberIDs}
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

	// Missing name.
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "rt1", "x", "y", nil, true)}); !strings.Contains(r, "缺少名称") {
		t.Fatalf("missing name must surface, got %q", r)
	}
	// Missing runtime_id → ask for a runtime.
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"无运行时", "", "x", "y", nil, true)}); !strings.Contains(r, "请指定 agent 的运行时") {
		t.Fatalf("missing runtime must ask, got %q", r)
	}
	// Hallucinated runtime_id → service-layer validator message.
	if r := d.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"坏runtime", "nope", "x", "y", nil, true)}); !strings.Contains(r, "创建 agent 失败") {
		t.Fatalf("hallucinated runtime must fail via the validator, got %q", r)
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
	if !strings.Contains(reply, "要给这个 agent 配吗") || !strings.Contains(reply, "git-helper") {
		t.Fatalf("must ask for skills and list them, got %q", reply)
	}
	// The draft is saved — the clarification turn must build from it.
	if _, ok := d2.loadAgentDraft(ctx); !ok {
		t.Fatal("agent-kind draft must be saved for the clarification")
	}
	// Clarification turn: owner picks skills → agent created from the draft.
	reply = d2.intakeCreateAgent(ctx, intakeAction{Intent: "create_agent", Agent: agentSub(
		"", "", "", "", []string{"sk1"}, true)})
	if !strings.Contains(reply, "已创建 agent") {
		t.Fatalf("clarification turn must create the agent, got %q", reply)
	}
	if _, ok := d2.loadAgentDraft(ctx); ok {
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

	// Missing name.
	if r := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"", "a1", "x", "y", nil)}); !strings.Contains(r, "缺少名称") {
		t.Fatalf("missing name must surface, got %q", r)
	}
	// Missing leader_id.
	if r := d.intakeCreateSquad(ctx, intakeAction{Intent: "create_squad", Squad: squadSub(
		"无leader", "", "x", "y", nil)}); !strings.Contains(r, "没有可用的 leader agent") {
		t.Fatalf("missing leader must ask, got %q", r)
	}
	// Hallucinated leader_id → service-layer validator message.
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