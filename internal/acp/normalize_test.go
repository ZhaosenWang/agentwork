package acp

import (
	"encoding/json"
	"testing"
)

// TestNormalizeNilSlicesOmitsHeadersAsEmptyArray: a nil Headers slice on an
// McpServer must serialize as [] (not vanish) — opencode's zod schema
// rejects MISSING arrays ("expected array, received undefined"), so the
// same session/new request must be accepted by strict and lenient servers
// alike. Live probe proved the regression: McpServers without headers →
// Invalid params; with headers: [] → accepted.
func TestNormalizeNilSlicesOmitsHeadersAsEmptyArray(t *testing.T) {
	req := NewSessionRequest{
		Cwd: "/tmp",
		McpServers: []McpServer{{
			Type: "http", Name: "agentwork", URL: "http://127.0.0.1:7373/mcp/test",
			// Headers intentionally nil.
		}},
	}
	raw, err := json.Marshal(normalizeNilSlices(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		McpServers []struct {
			Headers []any `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.McpServers) != 1 {
		t.Fatalf("mcpServers missing: %s", raw)
	}
	if decoded.McpServers[0].Headers == nil {
		t.Fatalf("headers must be emitted as an empty array, got missing: %s", raw)
	}
	if len(decoded.McpServers[0].Headers) != 0 {
		t.Fatalf("headers must be empty: %s", raw)
	}
}
