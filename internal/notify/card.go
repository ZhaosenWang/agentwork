package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Card construction (M3): Feishu interactive cards (JSON 2.0). Cards are the
// M3 upgrade over text pushes — the approval card carries the evidence and
// approve/reject buttons whose callbacks arrive over the long connection
// (card.action.trigger). The card content is emitted as a JSON string; the
// IM API is a content passthrough, so no SDK model dependency.

// buildReviewCard is the approval card (M3-1): header + gate reason +
// evidence summary + optional reject-reason input + approve/reject buttons.
// The buttons carry {action, goal_id, run_id} — the run id is the evidence
// run the card displays, recorded on the gate_decision (audit chain).
func buildReviewCard(g ReviewGoal) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**  \n`goal %s`", g.Title, short(g.GoalID))
	if g.Reason != "" {
		b.WriteString("\n卡点：" + g.Reason)
	}
	if ev := evidenceSummary(g.Evidence); ev != "" {
		b.WriteString("\n\n" + ev)
	}
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{"template": "orange", "title": map[string]any{
			"tag": "plain_text", "content": "🔔 待审批"}},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": b.String()}},
			map[string]any{"tag": "hr"},
			map[string]any{"tag": "input", "name": "reject_reason", "placeholder": "驳回理由（选填，驳回时建议填写）"},
			map[string]any{"tag": "action", "actions": []any{
				cardButton("✅ 批准", "primary", map[string]any{"action": "approve", "goal_id": g.GoalID, "run_id": g.RunID}),
				cardButton("❌ 驳回", "danger", map[string]any{"action": "reject", "goal_id": g.GoalID, "run_id": g.RunID}),
			}},
		},
	}
	raw, err := json.Marshal(card)
	return string(raw), err
}

// buildMilestoneCard is the generic milestone card (M3-2): a colored header
// (done green / failed red / merged blue) + title + body markdown.
func buildMilestoneCard(emoji, template, title, body string) (string, error) {
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{"template": template, "title": map[string]any{
			"tag": "plain_text", "content": emoji + " " + title}},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": body}},
		},
	}
	raw, err := json.Marshal(card)
	return string(raw), err
}

// buildProcessedCard replaces the approval card after a button decision: the
// buttons are gone, the outcome is stamped (M3-1, the Message.Update path).
func buildProcessedCard(goalID, decision string) (string, error) {
	ok := decision == "approve"
	header, body := "❌ 已驳回", "该卡点已驳回，goal 将带决策意见重跑。"
	if ok {
		header, body = "✅ 已批准", "平台正在自动合入（merge + 复验 + push）。"
	}
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{"template": map[bool]string{true: "green", false: "red"}[ok],
			"title": map[string]any{"tag": "plain_text", "content": header}},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": body + "  \n`goal " + short(goalID) + "`"}},
		},
	}
	raw, err := json.Marshal(card)
	return string(raw), err
}

func cardButton(text, typ string, value map[string]any) map[string]any {
	return map[string]any{
		"tag": "button",
		"text": map[string]any{"tag": "plain_text", "content": text},
		"type": typ,
		"value": value,
	}
}

// evidenceSummary renders the run.evidence JSON bundle into the approval
// card's markdown body: diff stat + verify outcome + agent summary. Unknown
// shapes degrade to an empty string (the card then shows only the gate
// reason — never a broken card).
func evidenceSummary(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var ev struct {
		DiffStat string   `json:"diff_stat"`
		Changed  []string `json:"changed"`
		Verify   string   `json:"verify"`
		Agent    string   `json:"agent"`
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return ""
	}
	var parts []string
	if s := strings.TrimSpace(ev.DiffStat); s != "" {
		// The diff stat is multi-line; keep the total line and drop the file
		// rows (card space).
		lines := strings.Split(s, "\n")
		parts = append(parts, "改动："+strings.TrimSpace(lines[len(lines)-1]))
	} else if n := len(ev.Changed); n > 0 {
		parts = append(parts, fmt.Sprintf("改动：%d 个文件", n))
	}
	if v := strings.TrimSpace(ev.Verify); v != "" {
		// Keep the command lines and the last output line (the outcome).
		lines := strings.Split(v, "\n")
		cmds, tail := []string{}, ""
		for i, l := range lines {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "$ ") {
				cmds = append(cmds, strings.TrimPrefix(l, "$ "))
			}
			if i == len(lines)-1 && l != "" {
				tail = l
			}
		}
		if len(cmds) > 0 {
			parts = append(parts, "verify: "+strings.Join(cmds, " | "))
		}
		if tail != "" {
			parts = append(parts, "结果："+truncate(tail, 120))
		}
	}
	if a := strings.TrimSpace(ev.Agent); a != "" {
		parts = append(parts, "agent："+truncate(a, 200))
	}
	return strings.Join(parts, "  \n")
}
