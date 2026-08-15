package feishu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/card"
)

func TestBuildCardBasic(t *testing.T) {
	c := &card.Card{
		Header:  card.CardHeader{Title: "Hello"},
		Content: "**bold** text",
		Color:   card.CardColorBlue,
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	assertPath(t, m, "header.title.content", "Hello")
	assertPath(t, m, "header.template", "blue")
	assertPath(t, m, "schema", "2.0")

	// Schema 2.0 — no config/wide_screen_mode (unless approval present).
	if _, ok := m["config"]; ok {
		t.Error("schema 2.0 should not have config")
	}

	// body.elements[0] is the markdown element.
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)
	first, _ := elems[0].(map[string]any)
	if first["tag"] != "markdown" {
		t.Errorf("first element tag = %v, want markdown", first["tag"])
	}
	if first["content"] != "**bold** text" {
		t.Errorf("first element content = %v, want **bold** text", first["content"])
	}
}

func TestBuildCardEmptyContent(t *testing.T) {
	c := &card.Card{
		Header: card.CardHeader{Title: "X"},
		Color:  card.CardColorGrey,
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}
	// Empty content should produce a placeholder element (hr), not "(empty)".
	if strings.Contains(result, "(empty)") {
		t.Errorf("empty content should not produce (empty), got: %s", result)
	}
	if !strings.Contains(result, `"tag":"hr"`) {
		t.Errorf("empty content should produce an hr placeholder, got: %s", result)
	}
}

func TestBuildCardWithFooter(t *testing.T) {
	c := &card.Card{
		Header:  card.CardHeader{Title: "X"},
		Content: "body",
		Footer:  "note text",
		Color:   card.CardColorGreen,
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"tag":"hr"`) {
		t.Error("footer should produce hr element")
	}
	if !strings.Contains(result, `"tag":"note"`) {
		t.Error("footer should produce note element")
	}
	if !strings.Contains(result, "note text") {
		t.Error("footer should contain note text")
	}
}

func TestBuildCardSubtitle(t *testing.T) {
	c := &card.Card{
		Header:  card.CardHeader{Title: "X", Subtitle: "sub"},
		Content: "body",
		Color:   card.CardColorRed,
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONContains(t, result, `"subtitle"`)
	assertJSONContains(t, result, `"sub"`)
}

func TestBuildCardAllColors(t *testing.T) {
	colors := map[card.CardColor]string{
		card.CardColorBlue:   "blue",
		card.CardColorGreen:  "green",
		card.CardColorRed:    "red",
		card.CardColorYellow: "yellow",
		card.CardColorOrange: "orange",
		card.CardColorPurple: "purple",
		card.CardColorGrey:   "grey",
	}
	for col, expected := range colors {
		c := &card.Card{Header: card.CardHeader{Title: "X"}, Content: "x", Color: col}
		result, err := BuildCard(c)
		if err != nil {
			t.Fatalf("color %s failed: %v", col, err)
		}
		if !strings.Contains(result, `"template":"`+expected+`"`) {
			t.Errorf("color %s: expected template %q in result", col, expected)
		}
	}
}

func TestBuildCardNil(t *testing.T) {
	_, err := BuildCard(nil)
	if err == nil {
		t.Fatal("expected error for nil card")
	}
}

func TestBuildCardWithApproval(t *testing.T) {
	c := &card.Card{
		Header:  card.CardHeader{Title: "🔧 调用工具中"},
		Content: "running...",
		Color:   card.CardColorYellow,
		Approval: &card.CardApproval{
			ToolName:   "shell",
			Args:       `{"command":"rm -rf /tmp"}`,
			ApprovalID: "abc123",
		},
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// config.update_multi should be set when approval is present.
	cfg, _ := m["config"].(map[string]any)
	if cfg == nil {
		t.Fatal("expected config when approval is set")
	}
	if cfg["update_multi"] != true {
		t.Error("expected update_multi=true")
	}

	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)

	// Last three elements: hr, markdown (approval context), column_set (buttons).
	n := len(elems)
	if n < 3 {
		t.Fatalf("expected at least 3 elements, got %d", n)
	}
	hr, _ := elems[n-3].(map[string]any)
	if hr["tag"] != "hr" {
		t.Errorf("expected hr before approval, got %v", hr["tag"])
	}
	md, _ := elems[n-2].(map[string]any)
	if md["tag"] != "markdown" {
		t.Errorf("expected markdown for approval context, got %v", md["tag"])
	}
	mdContent, _ := md["content"].(string)
	if !strings.Contains(mdContent, "shell") {
		t.Error("approval markdown should contain tool name")
	}
	if !strings.Contains(mdContent, "rm -rf /tmp") {
		t.Error("approval markdown should contain args")
	}
	colSet, _ := elems[n-1].(map[string]any)
	if colSet["tag"] != "column_set" {
		t.Errorf("expected column_set for buttons, got %v", colSet["tag"])
	}

	// Approval ID embedded in all 3 button values.
	count := strings.Count(result, `"approval_id":"abc123"`)
	if count != 3 {
		t.Errorf("expected 3 buttons with approval_id, got %d", count)
	}
}

func TestBuildCardWithoutApprovalHasNoConfig(t *testing.T) {
	c := &card.Card{
		Header:  card.CardHeader{Title: "X"},
		Content: "body",
		Color:   card.CardColorBlue,
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(result), &m)
	if _, ok := m["config"]; ok {
		t.Error("card without approval should not have config")
	}
}

func TestBuildCardTrimsContent(t *testing.T) {
	c := &card.Card{
		Header:  card.CardHeader{Title: "X"},
		Content: "  \n\n  hello  \n  ",
		Color:   card.CardColorBlue,
	}
	result, err := BuildCard(c)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONContains(t, result, `"content":"hello"`)
}

// ── Approval card tests ──

func TestBuildApprovalCard(t *testing.T) {
	cardJSON, err := BuildApprovalCard("abc123", "shell", `{"command":"rm -rf /tmp"}`)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(cardJSON), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	assertPath(t, m, "header.title.content", "需要权限")
	assertPath(t, m, "header.template", "blue")
	assertPath(t, m, "schema", "2.0")

	// No banner image — first body element is markdown, not img.
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)
	first, _ := elems[0].(map[string]any)
	if first["tag"] != "markdown" {
		t.Errorf("first element tag = %v, want markdown (no banner image)", first["tag"])
	}

	// Markdown contains tool name and args.
	md, _ := first["content"].(string)
	if !strings.Contains(md, "shell") {
		t.Error("markdown should contain tool name")
	}
	if !strings.Contains(md, "rm -rf /tmp") {
		t.Error("markdown should contain args")
	}

	// Approval ID embedded in all button values.
	if !strings.Contains(cardJSON, `"approval_id":"abc123"`) {
		t.Error("approval ID should be embedded in button values")
	}
	count := strings.Count(cardJSON, `"approval_id":"abc123"`)
	if count != 3 {
		t.Errorf("expected 3 buttons with approval_id, got %d", count)
	}
}

func TestBuildApprovalCardButtonStructure(t *testing.T) {
	cardJSON, err := BuildApprovalCard("x", "read", `{"path":"/etc"}`)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	json.Unmarshal([]byte(cardJSON), &m)

	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)

	// elements: [markdown, hr, column_set]
	if len(elems) != 3 {
		t.Fatalf("expected 3 body elements, got %d", len(elems))
	}
	if elems[1].(map[string]any)["tag"] != "hr" {
		t.Error("second element should be hr")
	}

	colSet, _ := elems[2].(map[string]any)
	if colSet["tag"] != "column_set" {
		t.Errorf("third element should be column_set, got %v", colSet["tag"])
	}

	cols, _ := colSet["columns"].([]any)
	if len(cols) != 3 {
		t.Fatalf("expected 3 columns (buttons), got %d", len(cols))
	}

	// No form or hint — Feishu schema V2 doesn't support them.
	if strings.Contains(cardJSON, `"tag":"form"`) {
		t.Error("card should not contain a form")
	}
	if strings.Contains(cardJSON, "/mode") {
		t.Error("card should not contain /mode hint")
	}
}

func TestBuildApprovalCardACPButtons(t *testing.T) {
	cardJSON, err := BuildApprovalCard("abc", "shell", `{"command":"ls"}`)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(cardJSON, "Allow Once") {
		t.Error("card should contain Allow Once button")
	}
	if !strings.Contains(cardJSON, "Allow Always") {
		t.Error("card should contain Allow Always button")
	}
	if !strings.Contains(cardJSON, "Deny") {
		t.Error("card should contain Deny button")
	}
	if !strings.Contains(cardJSON, `"action":"allow_once"`) {
		t.Error("card should contain allow_once action")
	}
	if !strings.Contains(cardJSON, `"action":"allow_always"`) {
		t.Error("card should contain allow_always action")
	}
	if !strings.Contains(cardJSON, `"action":"deny"`) {
		t.Error("card should contain deny action")
	}
}

func TestBuildResolvedCard(t *testing.T) {
	cardJSON, err := BuildResolvedCard("shell", `{"command":"ls"}`, "✅ **已同意**", "")
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	json.Unmarshal([]byte(cardJSON), &m)

	assertPath(t, m, "header.template", "green")
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)

	// First element should be a collapsible_panel (collapsed).
	p, _ := elems[0].(map[string]any)
	if p["tag"] != "collapsible_panel" {
		t.Errorf("expected collapsible_panel, got %v", p["tag"])
	}
	if expanded, _ := p["expanded"].(bool); expanded {
		t.Error("panel should start collapsed")
	}

	// Panel header title contains decision + tool name.
	hdr, _ := p["header"].(map[string]any)
	title, _ := hdr["title"].(map[string]any)
	content, _ := title["content"].(string)
	if !strings.Contains(content, "已同意") {
		t.Error("panel header should contain decision text")
	}
	if !strings.Contains(content, "shell") {
		t.Error("panel header should contain tool name")
	}

	// Panel body (hidden) contains the details.
	panelElems, _ := p["elements"].([]any)
	md, _ := panelElems[0].(map[string]any)
	mdContent, _ := md["content"].(string)
	if !strings.Contains(mdContent, "shell") {
		t.Error("panel body should contain tool name details")
	}

	// No form in resolved card.
	if strings.Contains(cardJSON, `"tag":"form"`) {
		t.Error("resolved card should not contain a form")
	}
}

func TestBuildResolvedCardDeny(t *testing.T) {
	cardJSON, err := BuildResolvedCard("shell", `{}`, "❌ **已拒绝**", "too dangerous")
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	json.Unmarshal([]byte(cardJSON), &m)

	assertPath(t, m, "header.template", "red")

	// Panel body should contain the rejection reason.
	body, _ := m["body"].(map[string]any)
	elems, _ := body["elements"].([]any)
	p, _ := elems[0].(map[string]any)
	panelElems, _ := p["elements"].([]any)
	md, _ := panelElems[0].(map[string]any)
	mdContent, _ := md["content"].(string)
	if !strings.Contains(mdContent, "too dangerous") {
		t.Error("panel body should contain rejection reason")
	}
}

// ── helpers ──

func assertPath(t *testing.T, m map[string]any, path, want string) {
	t.Helper()
	parts := strings.Split(path, ".")
	cur := any(m)
	for i, p := range parts {
		mp, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("at %q: expected map, got %T", strings.Join(parts[:i], "."), cur)
		}
		v, ok := mp[p]
		if !ok {
			t.Fatalf("path %q not found", path)
		}
		if i == len(parts)-1 {
			if s, ok := v.(string); !ok || s != want {
				t.Errorf("%s = %q, want %q", path, v, want)
			}
			return
		}
		cur = v
	}
}

func assertJSONContains(t testing.TB, jsonStr, substr string) {
	t.Helper()
	if !strings.Contains(jsonStr, substr) {
		t.Errorf("expected JSON to contain %q", substr)
	}
}
