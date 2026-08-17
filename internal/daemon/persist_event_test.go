package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/store"
)

// insertTestRun inserts a minimal run row so chat_message's run_id FK holds.
// Returns the run id.
func insertTestRun(t *testing.T, st *store.Store, goalID, agentID string) string {
	t.Helper()
	ctx := context.Background()
	id := "run-test"
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,status,role,attempt,result_summary,finished_at,queued_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, goalID, agentID, "worker", "worker", "running", "owner", 1,
		"", "", "", "2026-08-17T09:00:00Z"); err != nil {
		t.Fatalf("insert test run: %v", err)
	}
	return id
}

// TestPersistEventAggregatesToolCallByCallID: ACP emits multiple updates per
// tool call — a start (tool_call, maybe empty input), input-accumulation
// updates (tool_call_update), and a terminal update (status=completed →
// tool_result with output). The pre-aggregation code INSERTed a new
// chat_message row per update, so one tool call became 2–4 rows (the live
// "tool 出现两遍" symptom). The fix aggregates by CallID but KEEPS tool_use
// and tool_result as two separate rows (the feed renders them differently —
// call name+input vs output; collapsing them hid the call names). Multiple
// tool_use updates (input growing) merge into one tool_use row; the
// tool_result update lands as its own row.
func TestPersistEventAggregatesToolCallByCallID(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	runID := insertTestRun(t, st, goalID, agentID)

	callID := "call-1"
	// 1. start — empty/partial input, status pending → tool_use
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolUse, CallID: callID, Tool: "git status"})
	// 2. input accumulates → tool_use (input grew) — merges into the same tool_use row
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolUse, CallID: callID, Tool: "git status", Input: `{"command":"git status"}`})
	// 3. terminal → tool_result (output lands) — its own row
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolResult, CallID: callID, Tool: "git status", Input: `{"command":"git status"}`, Output: `{"exit":0,"output":"nothing to commit"}`})

	// TWO rows: one tool_use (aggregated from the 2 tool_use updates), one
	// tool_result. NOT 3 (the pre-aggregation symptom), NOT 1 (the collapsed
	// version that hid call names).
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_message WHERE run_id=? AND role='tool'`, runID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("one tool call = 1 tool_use row + 1 tool_result row = 2 rows, got %d", n)
	}

	// The tool_use row carries the LATEST input (accumulated), the tool_result
	// row carries the output. Query each by type.
	var useCalls, resCalls string
	rows, err := st.DB().QueryContext(ctx,
		`SELECT tool_calls FROM chat_message WHERE run_id=? AND role='tool' ORDER BY created_at`, runID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var tc string
		if err := rows.Scan(&tc); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var ev proto.Event
		if err := json.Unmarshal([]byte(tc), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", tc, err)
		}
		switch ev.Type {
		case proto.EventToolUse:
			useCalls = tc
		case proto.EventToolResult:
			resCalls = tc
		}
	}
	rows.Close()

	var useEv proto.Event
	if err := json.Unmarshal([]byte(useCalls), &useEv); err != nil {
		t.Fatalf("unmarshal tool_use %q: %v", useCalls, err)
	}
	if useEv.Type != proto.EventToolUse {
		t.Fatalf("tool_use row must stay tool_use, got %s", useEv.Type)
	}
	if !strings.Contains(useEv.Input, `"git status"`) {
		t.Fatalf("tool_use row must carry the accumulated (latest) input, got input=%q", useEv.Input)
	}

	var resEv proto.Event
	if err := json.Unmarshal([]byte(resCalls), &resEv); err != nil {
		t.Fatalf("unmarshal tool_result %q: %v", resCalls, err)
	}
	if resEv.Type != proto.EventToolResult {
		t.Fatalf("tool_result row must be tool_result, got %s", resEv.Type)
	}
	if !strings.Contains(resEv.Output, "nothing to commit") {
		t.Fatalf("tool_result row must carry the output, got output=%q", resEv.Output)
	}
	if resEv.CallID != callID {
		t.Fatalf("tool_result row must keep the CallID, got %q", resEv.CallID)
	}
}

// TestPersistEventDistinctToolCallsAreDistinctRows: two DIFFERENT tool calls
// (different CallIDs) produce four rows — tool_use + tool_result each.
// Aggregation is per (CallID, type), not global.
func TestPersistEventDistinctToolCallsAreDistinctRows(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	runID := insertTestRun(t, st, goalID, agentID)

	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolUse, CallID: "call-A", Tool: "ls"})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolResult, CallID: "call-A", Tool: "ls", Output: `{"exit":0}`})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolUse, CallID: "call-B", Tool: "pwd"})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolResult, CallID: "call-B", Tool: "pwd", Output: `{"exit":0}`})

	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_message WHERE run_id=? AND role='tool'`, runID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Fatalf("two distinct tool calls = 2 tool_use + 2 tool_result = 4 rows, got %d", n)
	}
}

// TestPersistEventToolCallNoCallIDFallsBack: a tool event with no CallID
// (older machine, malformed event) still renders — one row per event, no
// aggregation. Guards the fallback so a missing id never drops the event.
func TestPersistEventToolCallNoCallIDFallsBack(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	runID := insertTestRun(t, st, goalID, agentID)

	// Two events, no CallID — cannot aggregate, each gets its own row.
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolUse, Tool: "ls"})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolResult, Tool: "ls", Output: `{"exit":0}`})

	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chat_message WHERE run_id=? AND role='tool'`, runID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("no-CallID tool events must fall back to 1 row each (2 events → 2 rows), got %d", n)
	}
}

// TestPersistEventToolCallFlankedByText: a tool call between two assistant
// text chunks keeps all four rows in order — text before, the tool_use row,
// the tool_result row, text after. The text aggregator flushes when a tool
// event arrives, so the pre-tool text lands first; the trailing text lands
// on run-end flush.
func TestPersistEventToolCallFlankedByText(t *testing.T) {
	d, st, goalID, agentID := seedCtx(t)
	ctx := context.Background()
	runID := insertTestRun(t, st, goalID, agentID)

	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventMessage, Text: "Let me check "})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventMessage, Text: "the status."})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolUse, CallID: "c1", Tool: "git status"})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventToolResult, CallID: "c1", Tool: "git status", Output: `{"exit":0}`})
	d.persistEvent(ctx, runID, proto.Event{Type: proto.EventMessage, Text: "Done."})
	// The trailing text is buffered (aggregated) and lands on run-end flush —
	// the same path the executor takes when the run finishes.
	d.flushRunMessages(ctx, runID)

	rows, err := st.DB().QueryContext(ctx,
		`SELECT role FROM chat_message WHERE run_id=? ORDER BY created_at`, runID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	// Expect: assistant, tool(use), tool(result), assistant.
	if len(got) != 4 {
		t.Fatalf("expect 4 rows (text, tool_use, tool_result, text), got %d: %v", len(got), got)
	}
	if got[0] != "assistant" || got[1] != "tool" || got[2] != "tool" || got[3] != "assistant" {
		t.Fatalf("row order wrong, got %v", got)
	}
}
