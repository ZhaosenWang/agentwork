package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/service"
)

// IntakeService is the IM inbound pipeline (M3-4): the owner's natural-
// language message becomes a processor run whose parsed action (intake.json,
// file-as-side-effect — never parsed from agent stdout) the daemon executes
// and replies to. The parser agent is a global platform setting
// (platform.intake_agent) — processor agents are platform configuration,
// same as the acceptance-policy compiler (DESIGN.v2.md §5.3, decision 2-4).
//
// Triangle separation holds: the parser only understands intent and names
// ids; the PLATFORM executes the action (Create/Enqueue/query) and builds
// the reply — the agent never touches the store.
type IntakeService struct {
	qs     QueryStore
	store  SettingsStore
	runSvc *service.RunService
}

// intakeAgentKey is the app_settings blob holding the platform-wide M3
// settings (intake_agent + digest_time; owned by the settings API).
const intakeAgentKey = "platform.m3"

// intakeSettings is the JSON shape of that blob.
type intakeSettings struct {
	IntakeAgent string `json:"intake_agent"`
	DigestTime  string `json:"digest_time"`
}

func NewIntakeService(qs QueryStore, store SettingsStore, runSvc *service.RunService) *IntakeService {
	return &IntakeService{qs: qs, store: store, runSvc: runSvc}
}

// BuildPrompt assembles the parser instruction: the owner's raw message plus
// the platform capability contract and the agent/domain roster so the parser
// can resolve names to ids. Returns an error (with a user-facing message)
// when the global parser agent is not configured.
func (s *IntakeService) BuildPrompt(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("空消息")
	}
	var b strings.Builder
	b.WriteString("你是 agentwork 的 IM 入站解析器。用户通过飞书向 agentwork 发了一条消息，请解析其意图并写入当前工作目录的 intake.json 文件（文件即结果，不要输出到 stdout）。\n\n")
	b.WriteString("用户消息：\n" + text + "\n\n")
	b.WriteString(`intake.json 结构：
{
  "intent": "create_goal|review_list|goal_status|create_schedule|schedule_list|schedule_stop|unknown",
  "goal": {"title": "", "description": "", "assignee_id": "", "domain_id": ""},
  "goal_id": "",
  "schedule": {"name": "", "title": "", "description": "", "cron": "", "assignee_id": "", "domain_id": ""}
}

意图说明：
- create_goal：用户想创建/安排一个任务。title 必填（简洁的任务标题）；description 放细节；assignee_id 和 domain_id 从下面的名单里选最合适的 id（必须真实存在）；用户没指定时选最合理的。
- review_list：用户想查看待审批/卡点清单。不需要其他字段。
- goal_status：用户问某个任务/目标的状态。goal_id 填用户提到的 id（可能是短 id，照抄）。
- create_schedule：用户想创建定时任务/周期性任务（"每 1 个小时做 xxx"、"每天 9 点跑 xxx"、"每周一 xxx"）。schedule.name 填简短任务名；schedule.title 填每次触发时创建的任务标题；schedule.cron 把自然语言频率转成 5 段标准 cron 表达式；schedule.assignee_id 和 schedule.domain_id 从名单里选（必须真实存在）。
- schedule_list：用户想查看定时任务/计划任务清单。不需要其他字段。
- schedule_stop：用户想停掉/取消某个定时任务。schedule.name 填用户提到的定时任务名（照抄用户说的名字，可以不完全匹配）。
- unknown：无法归入以上意图（闲聊、问候、无关话题）。

cron 转换规则（自然语言频率 → 5 段 cron，时区按用户本地时间 Asia/Shanghai）：
- 每 N 分钟：*/N * * * *
- 每 N 小时：0 */N * * *
- 每小时：0 * * * *
- 每天 X 点：X 0 * * *  （X 是 0-23 的数字）
- 每天 X:Y：Y X * * *
- 每周一/二…X 点：X 0 * * 1-7（周一=1 … 周日=0 或 7）
- 每月 X 日 Y 点：Y X X * *
- 无法可靠转换就 intent=unknown，别编造 cron。

规则：intent 只能填上面七个值之一；id 只能从名单里选，不得编造；无法确定就 unknown。
`)
	b.WriteString("\n当前可用 agent（id: name）：\n")
	if agents, err := s.qs.Agents(ctx); err == nil {
		for _, a := range agents {
			fmt.Fprintf(&b, "- %s: %s\n", a.ID, a.Name)
		}
	}
	b.WriteString("\n当前可用 domain（id: name）：\n")
	if domains, err := s.qs.Domains(ctx); err == nil {
		for _, d := range domains {
			fmt.Fprintf(&b, "- %s: %s\n", d.ID, d.Name)
		}
	}
	b.WriteString("\n完成后用一句话说明解析依据。")
	return b.String(), nil
}

// Enqueue dispatches the intake parse run. msgID (the Feishu message id)
// doubles as the coalesce key — the same message redelivered must not spawn
// a second parse run.
func (s *IntakeService) Enqueue(ctx context.Context, msgID, prompt string) (*service.Run, error) {
	if s.runSvc == nil {
		return nil, errors.New("intake: runSvc not wired")
	}
	agentID, err := s.intakeAgent(ctx)
	if err != nil {
		return nil, err
	}
	return s.runSvc.EnqueueProcessorRun(ctx, "intake", msgID, agentID, prompt)
}

// intakeAgent resolves the configured global parser agent ('' + nil when
// unset — the caller surfaces the setup hint).
func (s *IntakeService) intakeAgent(ctx context.Context) (string, error) {
	raw, err := s.store.Get(ctx, intakeAgentKey)
	if err != nil {
		return "", fmt.Errorf("read intake settings: %w", err)
	}
	var st intakeSettings
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	if st.IntakeAgent == "" {
		return "", errors.New("未配置任务解析 agent（设置页 → 全局解析 Agent）")
	}
	return st.IntakeAgent, nil
}
