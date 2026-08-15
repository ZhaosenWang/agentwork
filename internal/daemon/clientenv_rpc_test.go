package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/acp"
)

// TestClientRPCWiring: Agent→Client RPC requests arriving on the transport
// are dispatched to the run's execution-environment handler (DESIGN.md
// 决策 4-8) — a real fs read and a real terminal command round-trip through
// the JSON-RPC layer, proving SetClientRequestHandler wiring end to end.
func TestClientRPCWiring(t *testing.T) {
	dir := t.TempDir()
	env := newRunEnvironment("run-1", "goal-1", "agent-1", dir, "http://127.0.0.1:7373", "tok-1")
	t.Cleanup(env.tm.cleanup)

	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello from worktree"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The fake agent "sends" two RPC requests up front: a file read and a
	// terminal spawn. The responses land in w in request order.
	readReq, _ := json.Marshal(map[string]any{"path": path})
	createReq, _ := json.Marshal(map[string]any{"command": "sh", "args": []string{"-c", "echo $AGENTWORK_RUN_ID"}})
	wire := "{\"jsonrpc\":\"2.0\",\"method\":\"fs/read_text_file\",\"params\":" + string(readReq) + ",\"id\":\"r1\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"method\":\"terminal/create\",\"params\":" + string(createReq) + ",\"id\":\"r2\"}\n"

	var w bytes.Buffer
	sess := acp.NewSession(strings.NewReader(wire), &w, func() error { return nil })
	sess.SetClientRequestHandler(env)

	// Wait for both responses (the second waits for the terminal's output to
	// be available, so allow generous time).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(w.String(), "r2") {
		time.Sleep(10 * time.Millisecond)
	}
	out := w.String()
	if !strings.Contains(out, "hello from worktree") {
		t.Fatalf("fs read response missing content:\n%s", out)
	}
	if !strings.Contains(out, "t1") {
		t.Fatalf("terminal create response missing terminal id:\n%s", out)
	}

	// The terminal was created through the RPC layer — poll it directly and
	// check the run context made it into the command's environment.
	tid := acp.TerminalId("t1")
	if _, err := env.tm.wait(tid); err != nil {
		t.Fatalf("wait: %v", err)
	}
	resp, _, _, err := env.tm.output(tid, nil)
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if !strings.Contains(resp.Output, "run-1") {
		t.Fatalf("terminal output %q missing injected AGENTWORK_RUN_ID", resp.Output)
	}
}
