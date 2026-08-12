package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// fakeHost is a test TerminalHost that actually runs commands (the daemon's
// terminalManager does the same thing; mcp cannot import daemon, so the test
// carries its own minimal implementation).
type fakeHost struct {
	mu    sync.Mutex
	terms map[string]*fakeTerm
	next  int
}

type fakeTerm struct {
	cmd      *exec.Cmd
	out      bytes.Buffer
	mu       sync.Mutex
	exited   bool
	code     int
	signal   string
	done     chan struct{}
	started  time.Time
}

func newFakeHost() *fakeHost {
	return &fakeHost{terms: map[string]*fakeTerm{}}
}

func (h *fakeHost) Create(command string, args []string, env []string, cwd string, byteLimit int) (acp.TerminalId, error) {
	if command == "" {
		return "", errEmptyCommand
	}
	cmd := exec.Command(command, args...)
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	t := &fakeTerm{cmd: cmd, done: make(chan struct{}), started: time.Now()}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	h.mu.Lock()
	h.next++
	id := acp.TerminalId("t" + string(rune('0'+h.next%10)) + string(rune('0'+h.next/10%10)))
	h.terms[string(id)] = t
	h.mu.Unlock()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				t.mu.Lock()
				t.out.Write(buf[:n])
				t.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		if werr := cmd.Wait(); werr != nil {
			if ee, ok := werr.(*exec.ExitError); ok {
				t.code = ee.ExitCode()
			} else {
				t.code = -1
			}
		}
		t.mu.Lock()
		t.exited = true
		t.mu.Unlock()
		close(t.done)
	}()
	return id, nil
}

func (h *fakeHost) Output(id acp.TerminalId, _ *int64) (*acp.TerminalOutputResponse, *int64, int64, error) {
	h.mu.Lock()
	t, ok := h.terms[string(id)]
	h.mu.Unlock()
	if !ok {
		return nil, nil, 0, errEmptyCommand
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resp := &acp.TerminalOutputResponse{Output: t.out.String(), Truncated: false}
	if t.exited {
		resp.ExitStatus = &acp.TerminalExitStatus{ExitCode: &t.code, Signal: &t.signal}
	}
	next := int64(t.out.Len())
	return resp, &next, int64(time.Since(t.started).Seconds()), nil
}

func (h *fakeHost) Release(id acp.TerminalId) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.terms[string(id)]; ok {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		delete(h.terms, string(id))
	}
	return nil
}

func newTestHandler(t *testing.T) (http.Handler, string) {
	dir := t.TempDir()
	exec := NewExecutor(dir, []string{"AGENTWORK_RUN_ID=run-test", "PATH=" + os.Getenv("PATH")}, newFakeHost())
	return HTTPHandler(exec), dir
}

func connect(t *testing.T, h http.Handler) *gmcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	cl := gmcp.NewClient(&gmcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := cl.Connect(ctx, &gmcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestMCPFullClientRoundTrip: the official SDK client drives the whole
// conversation (initialize → tools/list → tools/call) against the
// workspace server: fs tools + the async terminal trio.
func TestMCPFullClientRoundTrip(t *testing.T) {
	h, dir := newTestHandler(t)
	session := connect(t, h)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := map[string]bool{}
	for _, tl := range tools.Tools {
		found[tl.Name] = true
	}
	for _, want := range []string{"read_file", "write_file", "terminal_create", "terminal_output", "terminal_release"} {
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
	if tc, ok := res.Content[0].(*gmcp.TextContent); !ok || tc.Text != "via sdk client" {
		t.Fatalf("read content: want %q, got %+v", "via sdk client", res.Content[0])
	}

	// Async terminal: create → poll output until exited → release.
	create, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "terminal_create",
		Arguments: map[string]any{
			"command": "sh", "args": []any{"-c", "echo hello; exit 3"},
			"cwd":     dir,
		},
	})
	if err != nil {
		t.Fatalf("terminal_create: %v", err)
	}
	createText := create.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(createText, "terminal_id") {
		t.Fatalf("terminal_create: want terminal_id in %q", createText)
	}
	var tid string
	for _, part := range strings.Split(strings.Trim(createText, "{}"), ",") {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) == 2 && strings.Contains(kv[0], "terminal_id") {
			tid = strings.Trim(kv[1], `"`)
		}
	}
	if tid == "" {
		t.Fatalf("terminal_create: no id in %q", createText)
	}

	// Poll until exited (the fake host runs synchronously-ish).
	var out string
	for i := 0; i < 50; i++ {
		poll, err := session.CallTool(ctx, &gmcp.CallToolParams{
			Name: "terminal_output", Arguments: map[string]any{"terminal_id": tid},
		})
		if err != nil {
			t.Fatalf("terminal_output: %v", err)
		}
		pt := poll.Content[0].(*gmcp.TextContent).Text
		out = pt
		if strings.Contains(pt, `"exited":true`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("terminal_output: want command output, got %q", out)
	}
	if !strings.Contains(out, `"exit_code":3`) {
		t.Fatalf("terminal_output: want exit code 3, got %q", out)
	}

	if _, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "terminal_release", Arguments: map[string]any{"terminal_id": tid},
	}); err != nil {
		t.Fatalf("terminal_release: %v", err)
	}
}

// TestCollaborationTools: the collaboration tools act on the run's goal via
// the injected services (no CLI, no HTTP hop) — goal_comment lands a comment
// (mention parsing included), goal_list sees the goal.
func TestCollaborationTools(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	commentSvc := service.NewCommentService(st, bus)
	commentSvc.SetRunService(runSvc)
	commentSvc.SetGoalService(goalSvc)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)

	rt, _ := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	agentSvc := service.NewAgentService(st, bus)
	agentA, _ := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	agentB, _ := agentSvc.Create(ctx, service.Agent{Name: "b", RuntimeID: rt.ID})
	dom, _ := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	goal, _ := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})

	exec := NewExecutor(t.TempDir(), nil, newFakeHost())
	exec.SetCollaboration(goal.ID, agentA.ID, "run-1", commentSvc, goalSvc, runSvc, agentSvc, service.NewSquadService(st, bus))
	session := connect(t, HTTPHandler(exec))
	ctx2 := context.Background()

	// goal_comment with a mention → comment lands + a run is enqueued for B.
	if _, err := session.CallTool(ctx2, &gmcp.CallToolParams{
		Name: "goal_comment",
		Arguments: map[string]any{
			"content": "[@b](mention://agent/" + agentB.ID + ") please help",
		},
	}); err != nil {
		t.Fatalf("goal_comment: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx2, `SELECT COUNT(*) FROM comment WHERE goal_id=?`, goal.ID).Scan(&n); err != nil || n < 1 {
		t.Fatalf("comment not landed (n=%d err=%v)", n, err)
	}
	var pending int
	if err := st.DB().QueryRowContext(ctx2, `SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND status='queued'`, goal.ID, agentB.ID).Scan(&pending); err != nil || pending < 1 {
		t.Fatalf("mention run not enqueued (n=%d err=%v)", pending, err)
	}

	// goal_list sees the goal.
	res, err := session.CallTool(ctx2, &gmcp.CallToolParams{Name: "goal_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("goal_list: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, goal.ID) {
		t.Fatalf("goal_list missing our goal: %q", text)
	}

	// agent_list sees both agents.
	res, err = session.CallTool(ctx2, &gmcp.CallToolParams{Name: "agent_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("agent_list: %v", err)
	}
	text = res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, agentA.ID) || !strings.Contains(text, agentB.ID) {
		t.Fatalf("agent_list missing agents: %q", text)
	}
}
