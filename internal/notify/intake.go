package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/service"
)

// IntakeService is the IM inbound pipeline (M3-4): the owner's natural-
// language message becomes a processor run whose parsed action (intake.json,
// file-as-side-effect — never parsed from agent stdout) the daemon executes
// and replies to. The parser agent is a global platform setting
// (platform.intake_agent) — processor agents are platform configuration,
// same as the acceptance-policy compiler (DESIGN.md §5.3, decision 2-4).
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

// intakeDraft is the pending clarification (single-slot, single-user
// platform; expires after draftTTL). Two kinds share one slot:
//
//   - kind=""|"goal": a create_goal whose DOMAIN (a required parameter) the
//     parser could not determine. The owner is asked to name the repo; the
//     next inbound message is parsed WITH the draft as context, so a bare
//     "test-repo" completes the pending task instead of becoming a new one.
//
//   - kind="agent": a create_agent awaiting the owner's skills choice. The
//     owner is asked whether to select platform skills; the next message
//     resolves the skills (or declines) and the agent is created from the
//     draft's name/runtime/description/system_prompt — no need to re-send
//     the whole agent spec.
type IntakeDraft struct {
	Kind        string `json:"kind"` // ""|"goal" | "agent"
	Title       string `json:"title"`
	Description string `json:"description"`
	AssigneeID  string `json:"assignee_id"`
	// agent-kind fields (kind=="agent").
	AgentName         string `json:"agent_name"`
	AgentRuntimeID    string `json:"agent_runtime_id"`
	AgentDescription  string `json:"agent_description"`
	AgentSystemPrompt string `json:"agent_system_prompt"`
	CreatedAt         string `json:"created_at"`
}

const draftKey = "intake.draft"
const draftTTL = 10 * time.Minute

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
	// A pending clarification draft: the previous message created something
	// whose required parameter the user has not supplied yet. This message is
	// parsed IN THAT CONTEXT so a bare completion reply finishes the pending
	// item instead of starting a new one. Two kinds share one slot.
	if draft, ok := s.LoadDraft(ctx); ok {
		switch draft.Kind {
		case "agent":
			b.WriteString("【上下文：用户在回答之前 agent 创建的 skills 问题】\n")
			b.WriteString("之前解析出的 agent（已确认 name/runtime/description/system_prompt，只差 skills 选择）：\n")
			b.WriteString("- name：" + draft.AgentName + "\n")
			b.WriteString("- runtime_id：" + draft.AgentRuntimeID + "\n")
			if draft.AgentDescription != "" {
				b.WriteString("- description：" + draft.AgentDescription + "\n")
			}
			if draft.AgentSystemPrompt != "" {
				b.WriteString("- system_prompt：" + draft.AgentSystemPrompt + "\n")
			}
			b.WriteString("用户现在回复：\n\"" + text + "\"\n\n")
			b.WriteString("请判断：这条回复是否在选 skills（从下面的 skill 名单里选）？\n")
			b.WriteString("- 是（回复含 skill 名字）→ 输出 create_agent：name/runtime_id/description/system_prompt 照抄草稿，skills 填解析出的 id，skills_specified=true。\n")
			b.WriteString("- 否（用户说「不要/不用」，或开始了一个全新请求）→ 输出 create_agent：照抄草稿字段，skills 留空，skills_specified=true（明确不要也算指定）。\n\n")
		default: // "" | "goal"
			b.WriteString("【上下文：用户在补全之前的一条任务创建】\n")
			b.WriteString("之前解析出的任务（当时因未指定仓库而反问用户）：\n")
			if draft.Title != "" {
				b.WriteString("- 标题：" + draft.Title + "\n")
			}
			if draft.Description != "" {
				b.WriteString("- 描述：" + draft.Description + "\n")
			}
			b.WriteString("用户现在回复：\n\"" + text + "\"\n\n")
			b.WriteString("请判断：这条回复是否指定了仓库/域（从下面的域名名单里选）？\n")
			b.WriteString("- 是（回复是仓库名，或「在 X 上执行」等）→ 输出 create_goal：用草稿的标题/描述/assignee，domain_id 填选中的仓库。\n")
			b.WriteString("- 否（用户开始了一个全新任务——含「创建/做/加」等任务动词，或仍在闲聊）→ 按正常规则解析这条新消息。\n\n")
		}
	}
	b.WriteString("用户消息：\n" + text + "\n\n")
	b.WriteString(`intake.json 结构：
{
  "intent": "create_goal|review_list|goal_status|create_schedule|schedule_list|schedule_stop|create_agent|create_squad|unknown",
  "goal": {"title": "", "description": "", "assignee_id": "", "domain_id": ""},
  "goal_id": "",
  "schedule": {"name": "", "title": "", "description": "", "cron": "", "assignee_id": "", "domain_id": ""},
  "agent": {"name": "", "runtime_id": "", "description": "", "system_prompt": "", "skills": [], "skills_specified": false},
  "squad": {"name": "", "leader_id": "", "description": "", "instructions": "", "member_ids": []}
}

意图说明：
- create_goal：用户想创建/安排一个任务。title 必填（简洁的任务标题）；description 放细节；assignee_id 从下面的名单里选最合适的 id（必须真实存在）；用户没指定时选最合理的。
  title/description 用【直接指令口吻】写任务本身（如"优化 README 文档"），不要用"用户希望/用户想要"等转述口吻——执行 agent 看到的就是任务描述，不需要知道它是谁说的。
  domain_id 是【必要参数】：只有当用户的消息明确提到某个仓库/域（"在 test-repo 上做 xxx"、"给 agentwork 加个功能"）时才填；用户没明确指定时 domain_id 留空字符串（不要猜、不要选第一个）——平台会反问用户指定仓库。有多个可用域时尤其不能猜。
- review_list：用户想查看待审批/卡点清单。不需要其他字段。
- goal_status：用户问某个任务/目标的状态。goal_id 填用户提到的 id（可能是短 id，照抄）。
- create_schedule：用户想创建定时任务/周期性任务（"每 1 个小时做 xxx"、"每天 9 点跑 xxx"、"每周一 xxx"）。schedule.name 填简短任务名；schedule.title 填每次触发时创建的任务标题；schedule.cron 把自然语言频率转成 5 段标准 cron 表达式；schedule.assignee_id 和 schedule.domain_id 从名单里选（必须真实存在）。
- schedule_list：用户想查看定时任务/计划任务清单。不需要其他字段。
- schedule_stop：用户想停掉/取消某个定时任务。schedule.name 填用户提到的定时任务名（照抄用户说的名字，可以不完全匹配）。
- create_agent：用户想创建/配置一个 agent（含人设）。agent.name 必填（中文或英文短名）；agent.runtime_id 从下面的 runtime 名单里选 id（必填，必须真实存在）；agent.description 一句话描述（该 agent 做什么）；agent.system_prompt 是人设/角色说明（你是什么角色、怎么回答、有什么限制），用户没给也要塞一段合理默认值。
  agent.skills 从下面的 skill 名单里选 id——用户明确说"不要 skills"则留空数组；用户完全没提 skills 时 skills 留空数组且 skills_specified=false（平台会反问是否要 skills）。用户指定了 skills（哪怕一个）或明确说不要时 skills_specified=true。
  技术参数（env/model/mcp_servers/max_concurrent）不填（留空），用户后续在 Web 配置。
- create_squad：用户想创建一个 squad（团队）。squad.name 必填；squad.leader_id 从下面的 agent 名单里选 id（必填，必须真实存在）；squad.description 一句话描述；squad.instructions 是团队协作指令/规则（给 leader 看的子目标拆分与委派规则，用户没给也要塞一段合理默认值）；squad.member_ids 是成员 agent id 数组（不含 leader——leader 单独在 leader_id 里）。
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

规则：intent 只能填上面九个值之一；id 只能从名单里选，不得编造；无法确定就 unknown。
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
	b.WriteString("\n当前可用 runtime（id: name）：\n")
	if runtimes, err := s.qs.Runtimes(ctx); err == nil {
		for _, r := range runtimes {
			fmt.Fprintf(&b, "- %s: %s\n", r.ID, r.Name)
		}
	}
	b.WriteString("\n当前可用 skill（id: name）：\n")
	if skills, err := s.qs.Skills(ctx); err == nil {
		for _, sk := range skills {
			fmt.Fprintf(&b, "- %s: %s\n", sk.ID, sk.Name)
		}
	}
	b.WriteString("\n完成后用一句话说明解析依据。")
	return b.String(), nil
}

// Enqueue dispatches the intake parse run. The run is platform-level — it
// has no owning domain (the parser works from the prompt alone) — so
// domainID is "" and EnqueueProcessorRun coalesces on (run_type="intake",
// domain_id="", agent_id): a backlog of intake messages on the same parser
// agent folds into one queued run rather than duplicating. msgID (the Feishu
// message id) is NOT a coalesce key here — the connector already deduplicates
// inbound events, and stuffing msgID into the domain_id slot both broke
// coalescing (every message has a different id → never folds) and tripped
// nothing only because run.domain_id has no FK; semantically it was wrong.
func (s *IntakeService) Enqueue(ctx context.Context, msgID, prompt string) (*service.Run, error) {
	_ = msgID // retained in the signature for connector compatibility
	if s.runSvc == nil {
		return nil, errors.New("intake: runSvc not wired")
	}
	agentID, err := s.intakeAgent(ctx)
	if err != nil {
		return nil, err
	}
	return s.runSvc.EnqueueProcessorRun(ctx, "intake", "", agentID, prompt)
}

// SaveDraft stores the pending clarification (multi-domain task creation).
func (s *IntakeService) SaveDraft(ctx context.Context, d IntakeDraft) error {
	raw, _ := json.Marshal(d)
	return s.store.Set(ctx, draftKey, string(raw))
}

// ClearDraft drops the pending clarification (the task was created, or the
// owner gave up).
func (s *IntakeService) ClearDraft(ctx context.Context) error {
	return s.store.Delete(ctx, draftKey)
}

// LoadDraft returns the pending clarification if it exists and is fresh;
// an expired draft is cleared and treated as absent.
func (s *IntakeService) LoadDraft(ctx context.Context) (*IntakeDraft, bool) {
	raw, err := s.store.Get(ctx, draftKey)
	if err != nil || raw == "" {
		return nil, false
	}
	var d IntakeDraft
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, false
	}
	if t, err := time.Parse(time.RFC3339Nano, d.CreatedAt); err == nil && time.Since(t) > draftTTL {
		_ = s.store.Delete(ctx, draftKey)
		return nil, false
	}
	return &d, true
}

// intakeAgent resolves the configured global parser agent and verifies it
// still exists. A deleted agent (id left over in platform.m3 after a teardown
// or a re-seed) would otherwise fail at EnqueueProcessorRun with an opaque
// FOREIGN KEY error — the owner sees "解析任务创建失败：FOREIGN KEY
// constraint failed" and has no idea the configured agent is gone. Surface
// the real cause here, with the setup hint.
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
		logging.Warnf("intake: platform.m3 intake_agent unset — inbound IM messages will be rejected (Settings → 全局解析 Agent)")
		return "", errors.New("IM 解析 Agent 未配置或者已删除，请重新配置")
	}
	// The configured id must point at a live agent row — a stale config
	// (agent deleted, DB re-seeded) is the one setup drift that produces an
	// unreadable FK failure deep in EnqueueProcessorRun. AgentName returns
	// ("", nil) on miss; "" means the id is gone.
	if name, _ := s.qs.AgentName(ctx, st.IntakeAgent); name == "" {
		logging.Warnf("intake: platform.m3 intake_agent %s not found in agent table — reconfigure (Settings → 全局解析 Agent)", st.IntakeAgent)
		return "", errors.New("IM 解析 Agent 未配置或者已删除，请重新配置")
	}
	return st.IntakeAgent, nil
}
