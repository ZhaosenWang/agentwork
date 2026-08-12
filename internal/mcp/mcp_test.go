package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestHandler(t *testing.T) (http.Handler, string) {
	dir := t.TempDir()
	exec := NewExecutor(dir, []string{"AGENTWORK_RUN_ID=run-test", "PATH=" + os.Getenv("PATH")})
	return HTTPHandler(exec), dir
}

// TestMCPFullClientRoundTrip: the official SDK client drives the whole
// conversation (initialize → tools/list → tools/call) against the
// workspace server.
func TestMCPFullClientRoundTrip(t *testing.T) {
	h, dir := newTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx := context.Background()
	cl := gmcp.NewClient(&gmcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := cl.Connect(ctx, &gmcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := map[string]bool{}
	for _, tl := range tools.Tools {
		found[tl.Name] = true
	}
	for _, want := range []string{"read_file", "write_file", "run_command"} {
		if !found[want] {
			t.Fatalf("tool %q not advertised, got %v", want, found)
		}
	}

	// Write + read through the SDK client.
	path := filepath.Join(dir, "client-roundtrip.txt")
	if _, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "write_file",
		Arguments: map[string]any{"path": path, "content": "via sdk client"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "read_file", Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("read: no content: %+v", res)
	}
	if tc, ok := res.Content[0].(*gmcp.TextContent); !ok || tc.Text != "via sdk client" {
		t.Fatalf("read content: want %q, got %+v", "via sdk client", res.Content[0])
	}

	// A failing command surfaces the exit code as a tool-level error.
	fail, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "run_command", Arguments: map[string]any{
			"command": "sh", "args": []any{"-c", "exit 3"},
		},
	})
	if err != nil {
		t.Fatalf("failing command: %v", err)
	}
	if !fail.IsError {
		t.Fatalf("failing command must set IsError: %+v", fail)
	}
	if len(fail.Content) == 0 {
		t.Fatalf("failing command output: no content: %+v", fail.Content)
	}
	if tc, ok := fail.Content[0].(*gmcp.TextContent); !ok || !strings.Contains(tc.Text, "exit 3") {
		t.Fatalf("failing command output: want exit code, got %+v", fail.Content[0])
	}
}
