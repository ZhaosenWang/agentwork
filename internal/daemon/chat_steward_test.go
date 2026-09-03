package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// newStewardRosterDaemon builds a daemon with intakeSvc wired (for
// RosterBody) and a seeded domain + agent (for a non-empty roster).
func newStewardRosterDaemon(t *testing.T) *Daemon {
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
	ds := service.NewDomainService(st, bus)
	if _, err := ds.Create(ctx, service.Domain{Name: "d1", GitURL: "https://e.com/d1.git"}); err != nil {
		t.Fatal(err)
	}
	runSvc := service.NewRunService(st, bus)
	intakeSvc := notify.NewIntakeService(notify.NewSQLQueryStore(st), &mapSettings{vals: map[string]string{}}, runSvc)
	intakeSvc.SetAgentService(service.NewAgentService(st, bus))
	return &Daemon{
		st: st, bus: bus, qs: notify.NewSQLQueryStore(st),
		intakeSvc: intakeSvc,
	}
}

// TestInjectStewardRosterOnPrompt: a session/prompt frame gets a roster
// text block prepended to its prompt array; the user's original text block
// stays intact as the second element.
func TestInjectStewardRosterOnPrompt(t *testing.T) {
	d := newStewardRosterDaemon(t)
	e := &chatEntry{}
	original := `{"jsonrpc":"2.0","id":"1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"创建任务 优化README"}]}}`
	out := injectStewardRoster(d, e, []byte(original))
	if string(out) == original {
		t.Fatal("session/prompt must be rewritten, got unchanged frame")
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("rewrite produced invalid JSON: %v", err)
	}
	if len(msg.Params.Prompt) != 2 {
		t.Fatalf("prompt must have 2 blocks (roster + user), got %d", len(msg.Params.Prompt))
	}
	if !strings.Contains(msg.Params.Prompt[0].Text, "当前平台名单") {
		t.Fatalf("first block must be roster injection, got %q", msg.Params.Prompt[0].Text)
	}
	if !strings.Contains(msg.Params.Prompt[0].Text, "worker1") {
		t.Fatalf("roster must contain seeded agent, got %q", msg.Params.Prompt[0].Text)
	}
	if !strings.Contains(msg.Params.Prompt[0].Text, "d1") {
		t.Fatalf("roster must contain seeded domain, got %q", msg.Params.Prompt[0].Text)
	}
	if msg.Params.Prompt[1].Text != "创建任务 优化README" {
		t.Fatalf("user text must be preserved as second block, got %q", msg.Params.Prompt[1].Text)
	}
}

// TestInjectStewardRosterIgnoresOtherMethods: only session/prompt is
// rewritten; session/new, session/load, session/list, etc. pass through
// unchanged (return == input frame).
func TestInjectStewardRosterIgnoresOtherMethods(t *testing.T) {
	d := newStewardRosterDaemon(t)
	e := &chatEntry{}
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"s","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/list","params":{}}`,
	} {
		out := injectStewardRoster(d, e, []byte(frame))
		if string(out) != frame {
			t.Fatalf("non-session/prompt method must pass through unchanged, got %s", out)
		}
	}
}

// TestInjectStewardRosterNilIntakeSvc: when intakeSvc is nil (daemon not
// fully wired), the frame passes through unchanged.
func TestInjectStewardRosterNilIntakeSvc(t *testing.T) {
	d := &Daemon{}
	e := &chatEntry{}
	frame := `{"jsonrpc":"2.0","id":"1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"hi"}]}}`
	out := injectStewardRoster(d, e, []byte(frame))
	if string(out) != frame {
		t.Fatalf("nil intakeSvc must pass through unchanged, got %s", out)
	}
}

// TestInjectStewardRosterWithDispatchResult: when lastDispatchResult is set
// on the chatEntry, it is injected as a context block before the user's
// prompt, and cleared after injection.
func TestInjectStewardRosterWithDispatchResult(t *testing.T) {
	d := newStewardRosterDaemon(t)
	e := &chatEntry{lastDispatchResult: "📋 任务列表（1 个）：\n- 测试任务（abc12345） | [active] | 执行者：worker1"}
	original := `{"jsonrpc":"2.0","id":"1","method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"现在删掉"}]}}`
	out := injectStewardRoster(d, e, []byte(original))
	var msg struct {
		Params struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("rewrite produced invalid JSON: %v", err)
	}
	if len(msg.Params.Prompt) != 3 {
		t.Fatalf("prompt must have 3 blocks (roster + dispatch result + user), got %d", len(msg.Params.Prompt))
	}
	if !strings.Contains(msg.Params.Prompt[1].Text, "上次指令执行结果") {
		t.Fatalf("second block must be dispatch result, got %q", msg.Params.Prompt[1].Text)
	}
	if !strings.Contains(msg.Params.Prompt[1].Text, "abc12345") {
		t.Fatalf("dispatch result must contain task ID, got %q", msg.Params.Prompt[1].Text)
	}
	if msg.Params.Prompt[2].Text != "现在删掉" {
		t.Fatalf("user text must be preserved as last block, got %q", msg.Params.Prompt[2].Text)
	}
	if e.lastDispatchResult != "" {
		t.Fatal("lastDispatchResult must be cleared after injection")
	}
}

// TestStewardTurnCompleteClearsDraft: after dispatch processes a create_goal
// with missing fields (which saves a draft via collectAndAsk), the draft is
// cleared — the ACP chat path does not use the draft mechanism (steward
// handles multi-turn clarification conversationally).
func TestStewardTurnCompleteClearsDraft(t *testing.T) {
	d := newStewardRosterDaemon(t)
	ctx := context.Background()

	d.intakeCreateGoal(ctx, intakeAction{
		Intent: "create_goal",
		Goal:   goalSub("测试任务", "", "a1", ""),
	})
	if _, ok := d.loadDraftOfKind(ctx, "goal"); !ok {
		t.Fatal("collectAndAsk must have saved a goal draft")
	}

	// Simulate the ClearDraft that handleStewardTurnComplete does after dispatch.
	_ = d.intakeSvc.ClearDraft(ctx)
	if _, ok := d.loadDraftOfKind(ctx, "goal"); ok {
		t.Fatal("draft must be cleared after steward turn dispatch")
	}
}
