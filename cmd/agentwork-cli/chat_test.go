package main

import (
	"encoding/json"
	"testing"

	"github.com/eushing/agentwork/internal/acp"
)

// TestInjectChatMcpServersSessionNew: the rewriter replaces the web client's
// empty mcpServers with the agent's configured servers, preserving the
// frame's id/jsonrpc and any other params (cwd). This is the chat path's MCP
// injection point — the run path uses RunDispatchParams.McpServers at session
// new, but chat has no run dispatch, so the machine rewrites the frame.
func TestInjectChatMcpServersSessionNew(t *testing.T) {
	// A realistic session/new frame the web client sends (acp.ts:newSession
	// sends {mcpServers: []}; cwd is injected by the daemon's normalizeChatFrame).
	frame := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/new","params":{"cwd":"/home/u/.agentwork/chat/a1","mcpServers":[]}}`)
	mcp := []acp.McpServer{{Name: "browser", URL: "http://127.0.0.1:8080/sse", Type: "http"}}

	out := injectChatMcpServers(frame, mcp)
	if out == nil {
		t.Fatalf("session/new must be rewritten, got nil")
	}
	var got struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Cwd        string          `json:"cwd"`
			McpServers json.RawMessage `json:"mcpServers"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten frame is valid JSON: %v\n%s", err, out)
	}
	if got.JSONRPC != "2.0" || got.ID != 7 || got.Method != "session/new" {
		t.Fatalf("id/jsonrpc/method must be preserved, got %+v", got)
	}
	if got.Params.Cwd != "/home/u/.agentwork/chat/a1" {
		t.Fatalf("cwd param must be preserved, got %q", got.Params.Cwd)
	}
	var servers []acp.McpServer
	if err := json.Unmarshal(got.Params.McpServers, &servers); err != nil {
		t.Fatalf("mcpServers must be a valid array: %v\n%s", err, got.Params.McpServers)
	}
	if len(servers) != 1 || servers[0].Name != "browser" || servers[0].URL != "http://127.0.0.1:8080/sse" {
		t.Fatalf("mcpServers must be the agent's configured server, got %+v", servers)
	}
}

// TestInjectChatMcpServersNormalizesNilArrays: a stdio server (Type="", nil
// headers/env/args) must serialize headers/env as [] — NOT vanish. Strict ACP
// servers (opencode's zod) reject MISSING arrays ("expected array, received
// undefined"). The run path avoids this via the SDK's normalizeNilSlices; the
// chat relay rewrites the frame outside the SDK, so injectChatMcpServers must
// apply acp.NormalizeForWire to match. This is the regression for the live
// "Invalid params: headers/url/type/env received undefined" chat failure.
func TestInjectChatMcpServersNormalizesNilArrays(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"mcpServers":[]}}`)
	// stdio server exactly as the agent form stores it (type "" = stdio).
	mcp := []acp.McpServer{{Name: "local", Command: "foo", Args: []string{"bar"}}}

	out := injectChatMcpServers(frame, mcp)
	if out == nil {
		t.Fatalf("session/new must be rewritten, got nil")
	}
	var got struct {
		Params struct {
			McpServers []struct {
				Headers []any `json:"headers"`
				Env     []any `json:"env"`
				Args    []any `json:"args"`
				Name    string `json:"name"`
				Command string `json:"command"`
			} `json:"mcpServers"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten frame is valid JSON: %v\n%s", err, out)
	}
	if len(got.Params.McpServers) != 1 {
		t.Fatalf("expected 1 server, got %d: %s", len(got.Params.McpServers), out)
	}
	s := got.Params.McpServers[0]
	// The fix: nil slices must be present as [], not undefined.
	if s.Headers == nil {
		t.Fatalf("headers must be [] (not undefined) — strict servers reject missing arrays: %s", out)
	}
	if s.Env == nil {
		t.Fatalf("env must be [] (not undefined): %s", out)
	}
	if s.Args == nil || len(s.Args) != 1 {
		t.Fatalf("args must be preserved as [bar]: %s", out)
	}
}

// TestInjectChatMcpServersIgnoresOtherMethods: only session/new is rewritten —
// session/load, session/prompt, and every other method pass through untouched
// (nil), so the relay stays blind outside the one configured injection. This
// matches the cwd injection's discipline (normalizeChatFrame only touches
// session/new + session/load).
func TestInjectChatMcpServersIgnoresOtherMethods(t *testing.T) {
	mcp := []acp.McpServer{{Name: "x", URL: "u"}}
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"s","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/list","params":{}}`,
	} {
		if out := injectChatMcpServers([]byte(frame), mcp); out != nil {
			t.Fatalf("non-session/new method must return nil (pass through), got %s", out)
		}
	}
}

// TestInjectChatMcpServersMalformed: an unparseable frame degrades to nil
// (blind pass-through), never a dropped or corrupted frame.
func TestInjectChatMcpServersMalformed(t *testing.T) {
	mcp := []acp.McpServer{{Name: "x", URL: "u"}}
	if out := injectChatMcpServers([]byte("not json"), mcp); out != nil {
		t.Fatalf("malformed frame must return nil, got %s", out)
	}
}
