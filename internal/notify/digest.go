package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/card"
	"github.com/eushing/agentwork/internal/card/feishu"
)

// Daily digest (M3-3): the morning summary card — pending approvals +
// yesterday's completions + failures. Aggregated directly from the store,
// NOT from bus events: the bus is async fire-and-forget with no ordering
// guarantee, so an event-derived digest would be lossy. The daemon owns the
// schedule (a daily tick at the configured digest time, default 09:00) and
// the already-sent marker; this file only builds the card.

// BuildDigestCard aggregates the store into the digest card JSON. The
// completion window is [since, until) — "yesterday" — not open-ended: a
// goal finishing this morning must not appear in the morning digest twice.
// now is the digest timestamp for the card footer.
func BuildDigestCard(ctx context.Context, qs QueryStore, since, until time.Time, now time.Time) (string, error) {
	reviews, err := qs.ReviewGoals(ctx)
	if err != nil {
		return "", err
	}
	done, err := qs.TerminalSince(ctx, since.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	var body strings.Builder
	body.WriteString("**待审批**\n")
	if len(reviews) == 0 {
		body.WriteString("无\n")
	} else {
		for _, g := range reviews {
			fmt.Fprintf(&body, "- **%s**  \n  卡点：%s  \n", g.Title, firstLine(g.Reason))
		}
	}
	var completed, failed []string
	for _, b := range done {
		if b.Status == "done" {
			completed = append(completed, b.Title)
		} else {
			failed = append(failed, b.Title)
		}
	}
	body.WriteString("\n**昨日完成**\n")
	if len(completed) == 0 {
		body.WriteString("无\n")
	} else {
		for _, t := range completed {
			fmt.Fprintf(&body, "- %s\n", t)
		}
	}
	body.WriteString("\n**昨日失败 / 中断**\n")
	if len(failed) == 0 {
		body.WriteString("无\n")
	} else {
		for _, t := range failed {
			fmt.Fprintf(&body, "- %s\n", t)
		}
	}
	return feishu.BuildCard(&card.Card{
		Header:  card.CardHeader{Title: "📋 每日摘要 " + now.Format("01-02")},
		Content: body.String(),
		Footer:  "agentwork · 点开 Web 审批队列处理卡点",
		Color:   card.CardColorBlue,
	})
}

// firstLine keeps the card compact: a multi-line reason (reject notes can
// carry newlines) collapses to its first line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
