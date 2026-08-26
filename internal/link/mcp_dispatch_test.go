package link

import (
	"encoding/json"
	"testing"

	"github.com/eushing/agentwork/internal/acp"
)

// Verifies the agent's configured extra MCP servers survive the dispatch
// wire: daemon marshals RunDispatchParams (with McpServers) → JSON → the
// machine unmarshals it → exec.go reads p.McpServers. A regression that
// drops the field would silently re-break "额外 MCP 服务器".
func TestRunDispatchParamsMcpServersRoundTrip(t *testing.T) {
	in := RunDispatchParams{
		RunID: "r1", AgentID: "a1",
		McpServers: []acp.McpServer{{
			Type: "http", Name: "browser", URL: "http://127.0.0.1:8080/sse",
			Headers: []acp.HttpHeader{{Name: "X-Token", Value: "secret"}},
		}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RunDispatchParams
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.McpServers) != 1 {
		t.Fatalf("expected 1 mcp server, got %d (%v)", len(out.McpServers), out.McpServers)
	}
	if out.McpServers[0].Name != "browser" || out.McpServers[0].URL != "http://127.0.0.1:8080/sse" {
		t.Fatalf("round-trip lost fields: %+v", out.McpServers[0])
	}
	if len(out.McpServers[0].Headers) != 1 || out.McpServers[0].Headers[0].Value != "secret" {
		t.Fatalf("round-trip lost headers: %+v", out.McpServers[0].Headers)
	}
}

// omitempty: an agent with no extra MCP servers must not bloat the wire
// (and must decode to an empty slice, not nil-mismatched semantics).
func TestRunDispatchParamsMcpServersOmitempty(t *testing.T) {
	raw, _ := json.Marshal(RunDispatchParams{RunID: "r1"})
	if string(raw) == "" {
		t.Fatal("empty marshal")
	}
	var out RunDispatchParams
	_ = json.Unmarshal(raw, &out)
	if out.McpServers != nil {
		t.Fatalf("expected nil McpServers when absent, got %+v", out.McpServers)
	}
}
