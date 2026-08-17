package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/service"
)

// runIntakeTask executes an inbound-message parse run (M3-4): the parser
// agent reads the owner's message (the run prompt) and writes intake.json —
// the parsed action — into its scratch workdir. The PLATFORM executes the
// action (goal create / review list / goal status — never the agent; the
// parser only understands intent and names ids) and replies over IM.
//
// Structured output is read from the file, never from agent stdout
// (DESIGN.md §5.3, §9): the parser is a processor agent, same as the
// policy compiler.
func (d *Daemon) runIntakeTask(ctx context.Context, q *service.ClaimedRow, prompt, agentID string) {
	// Scratch workdir (no repo): the parser works from the prompt alone and
	// writes its result file here.
	workdir := filepath.Join(runsRoot(), "proc", q.RunID)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		d.failIntakeRun(ctx, q, "mkdir workdir: "+err.Error())
		return
	}
	// The artifact's ABSOLUTE path: the scratch dir is opaque to the agent —
	// told to "write intake.json in the current directory" it guessed (a
	// write_file call missing its required path arg, then a raw shell heredoc
	// terminal_create cannot run — command must be an executable). State the
	// full path so the write_file path argument is unambiguous.
	prompt += fmt.Sprintf("\n\nArtifact file ABSOLUTE path: %s\n(Write it there with your file tools; do NOT guess the working directory, do NOT use shell redirection)\n",
		filepath.Join(workdir, "intake.json"))
	var argsJSON, rtEnvJSON, intakeMachineID string
	var maxConcurrent int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.args, r.env, COALESCE(r.machine_id,''), a.max_concurrent
		 FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`, agentID).
		Scan(&argsJSON, &rtEnvJSON, &intakeMachineID, &maxConcurrent)
	if err != nil {
		d.failIntakeRun(ctx, q, "load agent runtime: "+err.Error())
		return
	}
	d.ensureWorker(agentID, maxConcurrent)

	var args []string
	_ = json.Unmarshal([]byte(argsJSON), &args)
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, agentID)

	// CLI 分支 Phase 2: machine-owned intake runtimes dispatch (same shape
	// as the compile processor — scratch proc dir + intake.json artifact).
	if intakeMachineID != "" {
		dispatchEnv := map[string]string{}
		for k, v := range rtEnv {
			dispatchEnv[k] = v
		}
		for k, v := range agentEnv {
			dispatchEnv[k] = v
		}
		d.dispatchToMachine(ctx, q, link.RunDispatchParams{
			RunID: q.RunID, AgentID: q.AgentID, Attempt: q.Attempt, Token: q.Token,
			Prompt: prompt, Proc: true, Scratch: true,
			ArtifactFiles: []string{"intake.json"},
			ACPSpawn: args, Env: dispatchEnv,
		}, intakeMachineID)
		return
	}

	// Legacy transports have no executor anymore (the unified model
	// dispatches everything to machines).
	d.failIntakeRun(ctx, q, "this runtime has no machine — run `agentwork connect` and point the agent at a machine-owned runtime")
}

// ingestIntakeArtifact completes an intake run from its FILE artifact
// (intake.json) — the shared path for local execution and the machine-
// dispatched upload (CLI 分支 Phase 2).
func (d *Daemon) ingestIntakeArtifact(ctx context.Context, q *service.ClaimedRow, artifactContent string) {
	var parsed intakeAction
	if err := json.Unmarshal([]byte(artifactContent), &parsed); err != nil {
		d.failIntakeRun(ctx, q, "parse intake.json: "+err.Error())
		return
	}
	d.replyIntake(ctx, q, parsed)
}

// failIntakeRun marks the parse run failed AND tells the owner — the inbound
// flow already acknowledged the message ("⏳ 收到"), so a silent failure
// would leave the user waiting for a result that never comes. The failure
// detail is sent IN FULL (no truncation): the user debugging a parse failure
// needs the whole reason, including the path that failed.
func (d *Daemon) failIntakeRun(ctx context.Context, q *service.ClaimedRow, summary string) {
	if n := d.imNotifier(); n != nil {
		if err := n.Send("⚠️ 消息解析失败：" + summary); err != nil {
			logging.Errorf("daemon: intake failure reply: %v", err)
		}
	}
	// Stamp the run failed directly (P0-5 conditional — a reaper stamp
	// wins) — NOT via failProcessorRun, whose domain:compile_failed event
	// is the compile path's signal and would mislabel an intake failure.
	res, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET status='failed', result_summary=?, finished_at=? WHERE id=? AND status IN ('queued','running')`,
		summary, nowStr(), q.RunID)
	if err != nil {
		logging.Infof("daemon: mark intake run %s failed: %v", q.RunID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		logging.Infof("daemon: intake run %s already terminal — dropping late failure", q.RunID)
	}
}

// intakeAction is the parser's output contract (see notify/intake.go's
// BuildPrompt for the shape the parser is instructed to produce).
type intakeAction struct {
	Intent string `json:"intent"`
	Goal   struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	} `json:"goal"`
	GoalID string `json:"goal_id"`
	// Schedule carries the parsed定时任务 fields (create_schedule / schedule_stop).
	Schedule struct {
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Cron        string `json:"cron"`
		AssigneeID  string `json:"assignee_id"`
		DomainID    string `json:"domain_id"`
	} `json:"schedule"`
	// Agent carries the create_agent fields. Technical config (env/model/
	// mcp_servers/max_concurrent) is deliberately absent — NL builds a
	// persona-bearing skeleton, the rest is filled in on the Web.
	Agent struct {
		Name            string   `json:"name"`
		RuntimeID       string   `json:"runtime_id"`
		Description     string   `json:"description"`
		SystemPrompt    string   `json:"system_prompt"`
		Skills          []string `json:"skills"`
		SkillsSpecified bool     `json:"skills_specified"`
	} `json:"agent"`
	// Squad carries the create_squad fields. Members are added after Create
	// via AddMember (role="member"); the leader is held in LeaderID, never in
	// MemberIDs.
	Squad struct {
		Name         string   `json:"name"`
		LeaderID     string   `json:"leader_id"`
		Description  string   `json:"description"`
		Instructions string   `json:"instructions"`
		MemberIDs    []string `json:"member_ids"`
	} `json:"squad"`
}

// replyIntake executes the parsed action and replies over IM. The run row is
// stamped completed here (the daemon owns processor-run finishing).
func (d *Daemon) replyIntake(ctx context.Context, q *service.ClaimedRow, parsed intakeAction) {
	var reply string
	switch parsed.Intent {
	case "create_goal":
		reply = d.intakeCreateGoal(ctx, parsed)
	case "review_list":
		reply = d.intakeReviewList(ctx)
	case "goal_status":
		reply = d.intakeGoalStatus(ctx, parsed.GoalID)
	case "create_schedule":
		reply = d.intakeCreateSchedule(ctx, parsed)
	case "schedule_list":
		reply = d.intakeScheduleList(ctx)
	case "schedule_stop":
		reply = d.intakeScheduleStop(ctx, parsed)
	case "create_agent":
		reply = d.intakeCreateAgent(ctx, parsed)
	case "create_squad":
		reply = d.intakeCreateSquad(ctx, parsed)
	default:
		reply = "没听懂这条指令 😅 你可以这样问我：\n- “创建任务 <标题>，让 <agent> 在 <domain> 上做 <描述>”\n- “查看待审批”\n- “查询任务状态 <id>”\n- “每 1 个小时做 <任务>”\n- “查看定时任务”\n- “停掉定时任务 <名字>”\n- “创建 agent <名字>，用 <运行时>，<人设描述>”\n- “创建 squad <名字>，leader 是 <agent>，成员有 <agent1> <agent2>”"
	}
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', result_summary=?, finished_at=? WHERE id=?`,
		reply, nowStr(), q.RunID); err != nil {
		logging.Infof("daemon: finish intake run %s: %v", q.RunID, err)
	}
	if n := d.imNotifier(); n != nil {
		if err := n.Send(reply); err != nil {
			logging.Errorf("daemon: intake reply: %v", err)
		}
	}
	logging.Infof("daemon: intake %s → %s", q.RunID, parsed.Intent)
}

// intakeCreateGoal creates the goal (active → first run enqueued) through the
// service layer — the goal layer validates assignee/domain, so a parser
// hallucinating an id fails here with the validator's message, not a
// platform crash.
func (d *Daemon) intakeCreateGoal(ctx context.Context, parsed intakeAction) string {
	g := parsed.Goal
	if strings.TrimSpace(g.Title) == "" {
		return "创建任务失败：缺少标题"
	}
	if strings.TrimSpace(g.DomainID) == "" {
		// Domain is a REQUIRED parameter (multi-domain): the parser was told
		// not to guess. Save the draft and ask the owner to name the repo —
		// the next inbound message is parsed with the draft as context, so a
		// bare repo name completes this task.
		if d.intakeSvc != nil {
			_ = d.intakeSvc.SaveDraft(ctx, notify.IntakeDraft{
				Title: g.Title, Description: g.Description, AssigneeID: g.AssigneeID,
				CreatedAt: nowStr(),
			})
		}
		return "这个任务需要在哪个项目执行？请回复项目名：\n" + d.intakeDomainList(ctx)
	}
	if strings.TrimSpace(g.AssigneeID) == "" {
		return "创建任务失败：没有可用的 agent（先在 Web 配置 agent）"
	}
	// P0-2 (决策 6-15②): the active goal's first run is born in Create's
	// transaction — no separate enqueue.
	created, err := d.goalSvc.Create(ctx, service.Goal{
		Title:         g.Title,
		Description:   g.Description,
		DomainID:      g.DomainID,
		AssigneeType:  "agent",
		AssigneeID:    g.AssigneeID,
		Status:        "active",
		CreatedByType: "human",
	})
	if err != nil {
		return "创建任务失败：" + err.Error()
	}
	// The pending clarification (if any) is resolved — the task exists now.
	if d.intakeSvc != nil {
		_ = d.intakeSvc.ClearDraft(ctx)
	}
	return fmt.Sprintf("✅ 已创建任务：%s（goal %s），agent 开始执行", created.Title, shortID(created.ID))
}

// intakeDomainList lists the available domains for the clarification ask.
func (d *Daemon) intakeDomainList(ctx context.Context) string {
	var b strings.Builder
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM domain ORDER BY name`)
	if err != nil {
		return "（当前没有可用项目——先在 Web 建域）"
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有可用项目——先在 Web 建域）"
	}
	return b.String()
}

// intakeReviewList answers "待审批" with the current checkpoint queue.
func (d *Daemon) intakeReviewList(ctx context.Context) string {
	if d.qs == nil {
		return "平台未就绪（store 未接线）"
	}
	goals, err := d.qs.ReviewGoals(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(goals) == 0 {
		return "✅ 当前没有待审批的卡点"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔔 待审批（%d 个）：\n", len(goals))
	for _, g := range goals {
		fmt.Fprintf(&b, "- %s（%s）\n  %s\n", g.Title, shortID(g.GoalID), firstLineIn(g.Reason))
	}
	b.WriteString("\n在飞书里点对应卡片的按钮，或打开 Web 审批队列处理。")
	return b.String()
}

// intakeGoalStatus answers "状态 <id>" (id or short id) with the goal's
// state and its last run's outcome.
func (d *Daemon) intakeGoalStatus(ctx context.Context, id string) string {
	if d.qs == nil {
		return "平台未就绪（store 未接线）"
	}
	if strings.TrimSpace(id) == "" {
		return "查询任务状态需要任务 id（如：查询任务状态 3f2a1b）"
	}
	v, err := d.qs.GoalStatus(ctx, strings.TrimSpace(id))
	if err != nil {
		return "查询失败：找不到该任务（" + err.Error() + "）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📌 %s（%s）\n状态：%s", v.Title, shortID(v.GoalID), v.Status)
	if v.ReviewRequest != "" {
		b.WriteString("\n待审：" + v.ReviewRequest)
	}
	if v.Summary != "" {
		b.WriteString("\n最近结果：" + truncateIn(v.Summary, 200))
	}
	return b.String()
}

// intakeCreateSchedule creates a cron schedule through the service layer —
// the parser converts natural-language frequency to cron, the platform
// validates (cron syntax, assignee/domain existence) and computes the first
// next_run_at.
func (d *Daemon) intakeCreateSchedule(ctx context.Context, parsed intakeAction) string {
	sch := parsed.Schedule
	if strings.TrimSpace(sch.Name) == "" || strings.TrimSpace(sch.Title) == "" {
		return "创建定时任务失败：缺少任务名或任务标题"
	}
	if strings.TrimSpace(sch.Cron) == "" {
		return "创建定时任务失败：缺少 cron 表达式（没听懂频率？）"
	}
	if strings.TrimSpace(sch.DomainID) == "" {
		return "创建定时任务失败：没有可用的 domain（先在 Web 建域并配置验收策略）"
	}
	if strings.TrimSpace(sch.AssigneeID) == "" {
		return "创建定时任务失败：没有可用的 agent（先在 Web 配置 agent）"
	}
	// The schedule runs on the daemon machine's OWN local time — the owner
	// speaks in their local hours ("每天 9 点"), and on a single-user machine
	// that IS the daemon's zone. Hardcoding a zone (e.g. Asia/Shanghai)
	// silently mis-times every schedule on a machine in another zone.
	timezone := time.Local.String()
	if timezone == "" {
		timezone = "UTC"
	}
	s, err := d.schedSvc.Create(ctx, service.Schedule{
		Name:           sch.Name,
		TitleTemplate:  sch.Title,
		Description:    sch.Description,
		AssigneeType:   "agent",
		AssigneeID:     sch.AssigneeID,
		DomainID:       sch.DomainID,
		CronExpression: sch.Cron,
		Timezone:       timezone,
		Enabled:        true,
	})
	if err != nil {
		return "创建定时任务失败：" + err.Error()
	}
	next := s.NextRunAt
	if next != "" {
		if t, err := time.Parse(time.RFC3339Nano, next); err == nil {
			next = t.Local().Format("01-02 15:04")
		}
	}
	return fmt.Sprintf("✅ 已创建定时任务：%s（%s），下次执行 %s（本地时间）", s.Name, s.CronExpression, next)
}

// intakeScheduleList answers "查看定时任务" with the enabled schedules.
func (d *Daemon) intakeScheduleList(ctx context.Context) string {
	all, err := d.schedSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	enabled := []service.Schedule{}
	for _, s := range all {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	if len(enabled) == 0 {
		return "📭 当前没有启用的定时任务"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📅 启用的定时任务（%d 个）：\n", len(enabled))
	for _, s := range enabled {
		fmt.Fprintf(&b, "- %s（%s）\n", s.Name, s.CronExpression)
		if s.Description != "" {
			b.WriteString("  " + firstLineIn(s.Description) + "\n")
		}
	}
	b.WriteString("\n停掉某个：发送“停掉定时任务 <名字>”")
	return b.String()
}

// intakeScheduleStop disables a schedule by name (the row and firing history
// stay; dispatchSchedules only fires enabled rows).
func (d *Daemon) intakeScheduleStop(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Schedule.Name)
	if name == "" {
		return "停掉定时任务需要名字（如：停掉定时任务 每小时巡检）"
	}
	all, err := d.schedSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	var target *service.Schedule
	for i := range all {
		if all[i].Enabled && all[i].Name == name {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("没找到启用的定时任务 %q——先“查看定时任务”确认名字", name)
	}
	if _, err := d.schedSvc.SetEnabled(ctx, target.ID, false); err != nil {
		return "停用失败：" + err.Error()
	}
	return fmt.Sprintf("⏹ 已停用定时任务：%s（%s）", target.Name, target.CronExpression)
}

// intakeCreateAgent creates an agent (persona + runtime + optional skills)
// through the service layer. Agents are platform-wide (no domain_id), so
// unlike create_goal there is no domain clarification — but skills has its
// own clarification: if the owner did not say whether the agent should get
// platform skills AND the library is non-empty, the platform asks first.
func (d *Daemon) intakeCreateAgent(ctx context.Context, parsed intakeAction) string {
	a := parsed.Agent
	// 1. Clarification turn: an agent-kind draft means the owner is replying
	// to the skills question. Build from the draft's agent spec + this turn's
	// parsed skills (empty is fine — a vague reply still creates the agent,
	// no second ask).
	if draft, ok := d.loadAgentDraft(ctx); ok {
		created, err := d.agentSvc.Create(ctx, service.Agent{
			Name:         draft.AgentName,
			RuntimeID:    draft.AgentRuntimeID,
			Description:  draft.AgentDescription,
			SystemPrompt: draft.AgentSystemPrompt,
			Skills:       a.Skills,
		})
		if d.intakeSvc != nil {
			_ = d.intakeSvc.ClearDraft(ctx)
		}
		if err != nil {
			return "创建 agent 失败：" + err.Error()
		}
		return fmt.Sprintf("✅ 已创建 agent：%s（%s）", created.Name, shortID(created.ID))
	}
	// 2. Fresh create.
	if strings.TrimSpace(a.Name) == "" {
		return "创建 agent 失败：缺少名称"
	}
	if strings.TrimSpace(a.RuntimeID) == "" {
		return "请指定 agent 的运行时，当前可用：\n" + d.intakeRuntimeList(ctx)
	}
	// Skills clarification: the owner did not mention skills at all AND the
	// platform has skills → ask once. Save the agent spec as a draft so the
	// owner's reply only has to name skills, not re-send the whole agent.
	if !a.SkillsSpecified && d.platformHasSkills(ctx) {
		if d.intakeSvc != nil {
			_ = d.intakeSvc.SaveDraft(ctx, notify.IntakeDraft{
				Kind:              "agent",
				AgentName:         a.Name,
				AgentRuntimeID:    a.RuntimeID,
				AgentDescription:  a.Description,
				AgentSystemPrompt: a.SystemPrompt,
				CreatedAt:         nowStr(),
			})
		}
		return "平台有以下 skill，要给这个 agent 配吗？回复 skill 名字（空格分隔），或回复“不要”：\n" + d.intakeSkillList(ctx)
	}
	created, err := d.agentSvc.Create(ctx, service.Agent{
		Name:         a.Name,
		RuntimeID:    a.RuntimeID,
		Description:  a.Description,
		SystemPrompt: a.SystemPrompt,
		Skills:       a.Skills,
	})
	if err != nil {
		return "创建 agent 失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已创建 agent：%s（%s）", created.Name, shortID(created.ID))
}

// intakeCreateSquad creates a squad (leader + optional members) through the
// service layer, then attaches the members. Squads are platform-wide and have
// no skills field, so no clarification is needed. Members that fail to attach
// (a hallucinated id) surface as a partial-success reply — the squad exists,
// the owner is told which members did not make it.
func (d *Daemon) intakeCreateSquad(ctx context.Context, parsed intakeAction) string {
	sq := parsed.Squad
	if strings.TrimSpace(sq.Name) == "" {
		return "创建 squad 失败：缺少名称"
	}
	if strings.TrimSpace(sq.LeaderID) == "" {
		return "创建 squad 失败：没有可用的 leader agent（先通过 Web 创建 agent，或在消息里指明 leader 名字）"
	}
	created, err := d.squadSvc.Create(ctx, service.Squad{
		Name:         sq.Name,
		LeaderID:     sq.LeaderID,
		Description:  sq.Description,
		Instructions: sq.Instructions,
	})
	if err != nil {
		return "创建 squad 失败：" + err.Error()
	}
	// Attach members (skip the leader — it is already squad.leader_id).
	var failed []string
	for _, mid := range sq.MemberIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" || mid == sq.LeaderID {
			continue
		}
		if _, err := d.squadSvc.AddMember(ctx, created.ID, "agent", mid, "member"); err != nil {
			failed = append(failed, mid)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("⚠️ 已创建 squad：%s（%s），但以下成员添加失败：%s", created.Name, shortID(created.ID), strings.Join(failed, "、"))
	}
	return fmt.Sprintf("✅ 已创建 squad：%s（%s）", created.Name, shortID(created.ID))
}

// loadAgentDraft returns the pending agent-kind clarification draft, if any.
// A goal-kind draft (or none) is treated as no agent draft — the two flows
// do not interfere.
func (d *Daemon) loadAgentDraft(ctx context.Context) (*notify.IntakeDraft, bool) {
	if d.intakeSvc == nil {
		return nil, false
	}
	draft, ok := d.intakeSvc.LoadDraft(ctx)
	if !ok || draft.Kind != "agent" {
		return nil, false
	}
	return draft, true
}

// platformHasSkills reports whether the skill library is non-empty — the
// skills clarification only fires when there is something to choose.
func (d *Daemon) platformHasSkills(ctx context.Context) bool {
	var n int
	if err := d.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM skill`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// intakeRuntimeList lists the active runtimes for the clarification ask.
func (d *Daemon) intakeRuntimeList(ctx context.Context) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM runtime WHERE status='active' ORDER BY name`)
	if err != nil {
		return "（当前没有可用运行时——先在 Web 配置 runtime）"
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有可用运行时——先在 Web 配置 runtime）"
	}
	return b.String()
}

// intakeSkillList lists the platform skill library for the clarification ask.
func (d *Daemon) intakeSkillList(ctx context.Context) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM skill ORDER BY name`)
	if err != nil {
		return "（查询 skill 失败）"
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有 skill）"
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func firstLineIn(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func truncateIn(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
