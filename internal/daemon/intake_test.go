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
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc, runSvc: runSvc, schedSvc: schedSvc, qs: notify.NewSQLQueryStore(st)}
	return d, st
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
