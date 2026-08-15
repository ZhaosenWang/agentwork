// Package card defines a platform-neutral structured-message model.
//
// A Card carries a header (title + subtitle + accent colour), a markdown
// body, an optional footer note, and nested collapsible panels. Each
// platform adapter translates the neutral Card into its native card format
// (e.g. the feishu sub-package produces Feishu interactive-card JSON
// schema 2.0).
//
// The model intentionally knows nothing about IM connections, sessions, or
// LLMs — it is pure presentation data passed to a renderer.
package card

import "strings"

// FoldMode controls how a card body is rendered on platforms that support
// collapsible content (e.g. Feishu collapsible_panel).
type FoldMode int

const (
	// FoldNone renders the body as plain markdown — always visible, no fold
	// affordance. This is the zero value and the most common case.
	FoldNone FoldMode = iota
	// FoldCollapsed wraps the body in a collapsible panel that starts
	// collapsed (hidden behind a clickable bar).
	FoldCollapsed
	// FoldExpanded wraps the body in a collapsible panel that starts
	// expanded (visible, but the user can click to collapse).
	FoldExpanded
)

// Card is a platform-neutral structured message. Each adapter translates it
// into the platform's native card format (e.g. Feishu interactive card).
type Card struct {
	Header   CardHeader
	Content  string // markdown body
	Footer   string // optional note at the bottom
	Color    CardColor
	Fold     FoldMode       // how the body wraps: none, collapsed panel, or expanded panel
	Panels   []Card         // nested collapsed sub-panels (when non-empty, Content is ignored)
	Approval *CardApproval  // optional approval section rendered at the bottom
}

// CardApproval carries the info needed to render an approval button row
// inside a card. When set, BuildCard appends a separator, a context line
// (tool name + args), and three action buttons (allow once / allow always /
// deny) to the card body. The ApprovalID is embedded in every button's value
// so the card action callback can correlate clicks back to the pending
// approval.
type CardApproval struct {
	ToolName   string
	Args       string
	ApprovalID string
}

// CardHeader is the title area of a card.
type CardHeader struct {
	Title    string
	Subtitle string
	// TitleMarkdown renders the title with lark_md (Feishu markdown) instead
	// of plain_text, allowing **bold** etc. Used by tool-call panel titles to
	// bold the first word.
	TitleMarkdown bool
}

// CardColor controls the header accent colour.
type CardColor string

const (
	CardColorBlue   CardColor = "blue"
	CardColorGreen  CardColor = "green"
	CardColorRed    CardColor = "red"
	CardColorYellow CardColor = "yellow"
	CardColorOrange CardColor = "orange"
	CardColorPurple CardColor = "purple"
	CardColorGrey   CardColor = "grey"
)

// CodeBlock wraps content in a markdown code fence long enough to safely
// enclose any backtick run appearing inside content. Per CommonMark spec, a
// fence of N backticks cannot be closed by a run of fewer than N backticks,
// so inner ``` markers are rendered as literal text instead of prematurely
// terminating the block. This prevents markdown constructs inside tool
// output (e.g. tables in a SKILL.md) from leaking out and tripping platform
// card limits (Feishu ErrCode 11310).
func CodeBlock(content string) string {
	maxRun, run := 0, 0
	for _, c := range content {
		if c == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	n := 3
	if maxRun >= n {
		n = maxRun + 1
	}
	fence := strings.Repeat("`", n)
	return fence + "\n" + content + "\n" + fence
}
