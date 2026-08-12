package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/proto/acpbackend"
)

// fakeACPAgent is a minimal ACP agent: during its single prompt turn it
// exercises the execution-environment proxy end to end — fs read, terminal
// spawn, wait, output — through real Agent→Client RPCs, and records what
// it observed for the test to assert.
type fakeACPAgent struct {
	requester acp.ClientRequester
	cwd       string

	// Observations from inside OnPrompt.
	readContent string
	termOutput  string
	termExit    *int
	termSignal  *string
	rpcErrs     []string

	// Handshake observation.
	clientCaps acp.ClientCapabilities
	// Session observation: the advertised MCP servers.
	mcpServers []acp.McpServer
}

func (a *fakeACPAgent) SetClientRequester(r acp.ClientRequester) { a.requester = r }

func (a *fakeACPAgent) OnInitialize(ctx context.Context, req acp.InitializeRequest) (*acp.InitializeResponse, error) {
	a.clientCaps = req.ClientCapabilities
	return &acp.InitializeResponse{ProtocolVersion: 1, AgentCapabilities: acp.AgentCapabilities{}}, nil
}
func (a *fakeACPAgent) OnNewSession(ctx context.Context, req acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	a.cwd = req.Cwd
	a.mcpServers = req.McpServers
	return &acp.NewSessionResponse{SessionID: "s1"}, nil
}
func (a *fakeACPAgent) OnLoadSession(ctx context.Context, req acp.LoadSessionRequest, sender acp.SessionEventSender) (*acp.LoadSessionResponse, error) {
	return nil, errors.New("unused")
}
func (a *fakeACPAgent) OnResumeSession(ctx context.Context, req acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
	return nil, errors.New("unused")
}
func (a *fakeACPAgent) OnCloseSession(ctx context.Context, req acp.CloseSessionRequest) (*acp.CloseSessionResponse, error) {
	return &acp.CloseSessionResponse{}, nil
}
func (a *fakeACPAgent) OnDeleteSession(ctx context.Context, req acp.DeleteSessionRequest) (*acp.DeleteSessionResponse, error) {
	return &acp.DeleteSessionResponse{}, nil
}
func (a *fakeACPAgent) OnListSessions(ctx context.Context, req acp.ListSessionsRequest) (*acp.ListSessionsResponse, error) {
	return &acp.ListSessionsResponse{}, nil
}
func (a *fakeACPAgent) OnSetSessionMode(ctx context.Context, req acp.SetSessionModeRequest) (*acp.SetSessionModeResponse, error) {
	return &acp.SetSessionModeResponse{}, nil
}
func (a *fakeACPAgent) OnSetSessionConfigOption(ctx context.Context, req acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	return &acp.SetSessionConfigOptionResponse{}, nil
}
func (a *fakeACPAgent) OnPrompt(ctx context.Context, req acp.PromptRequest, sender acp.SessionEventSender) (*acp.PromptResponse, error) {
	// The turn's work: read the worktree through the proxy, then run a
	// terminal command and confirm the injected run context in its env.
	read, err := a.requester.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: filepath.Join(a.cwd, "a.txt")})
	if err != nil {
		a.rpcErrs = append(a.rpcErrs, "read: "+err.Error())
	} else {
		a.readContent = read.Content
	}

	tid, err := a.requester.CreateTerminal(ctx, acp.CreateTerminalRequest{
		Command: "sh",
		Args:    []string{"-c", "echo $AGENTWORK_RUN_ID"},
	})
	if err != nil {
		a.rpcErrs = append(a.rpcErrs, "create: "+err.Error())
	} else {
		if wait, err := a.requester.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{TerminalID: tid.TerminalID}); err != nil {
			a.rpcErrs = append(a.rpcErrs, "wait: "+err.Error())
		} else {
			a.termExit, a.termSignal = wait.ExitCode, wait.Signal
		}
		out, err := a.requester.TerminalOutput(ctx, acp.TerminalOutputRequest{TerminalID: tid.TerminalID})
		if err != nil {
			a.rpcErrs = append(a.rpcErrs, "output: "+err.Error())
		} else {
			a.termOutput = out.Output
		}
	}

	_ = sender.SendAgentMessage("work done")
	return &acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}
func (a *fakeACPAgent) OnLogout(ctx context.Context, req acp.LogoutRequest) (*acp.LogoutResponse, error) {
	return &acp.LogoutResponse{}, nil
}
func (a *fakeACPAgent) OnCancel(ctx context.Context, sid acp.SessionId) error { return nil }
func (a *fakeACPAgent) OnAuthenticate(ctx context.Context, req acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
	return &acp.AuthenticateResponse{}, nil
}

// TestExecuteFullRoundTrip: acpbackend.Execute against a REAL ACP server,
// with the run's execution-environment proxy wired as the handler — the
// agent's fs/terminal RPCs execute on the platform side and the run
// context reaches the spawned command's environment.
func TestExecuteFullRoundTrip(t *testing.T) {
	dir := t.TempDir()
	env := newRunEnvironment("run-1", "goal-1", "agent-1", dir, "http://127.0.0.1:7373")
	t.Cleanup(env.tm.cleanup)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello worktree"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wire the ACP server (fake agent) to the client-side backend over
	// pipes — a real JSON-RPC conversation in both directions.
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &fakeACPAgent{}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- acp.NewServer("fake-agent", "1.0", agent).RunTransport(ctx, serverW, serverR)
	}()

	conn := proto.Conn{
		R: clientR,
		W: clientW,
		Close: func() error {
			cancel()
			clientR.CloseWithError(nil)
			clientW.CloseWithError(nil)
			return nil
		},
	}

	backend := acpbackend.New()
	run, err := backend.Execute(ctx, proto.ExecuteSpec{
		Conn:          conn,
		Cwd:           dir,
		Prompt:        "do the work",
		ClientHandler: env,
		McpServers: []acp.McpServer{{
			Type: "http", Name: "workspace", URL: "http://127.0.0.1:7373/mcp/run-1",
		}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var messages []string
	for ev := range run.Events {
		if ev.Type == proto.EventMessage {
			messages = append(messages, ev.Text)
		}
	}
	result, ok := <-run.Result
	conn.Close()
	if !ok {
		t.Fatal("result channel closed without a value")
	}
	if result.Status != proto.StatusCompleted {
		t.Fatalf("result status: want completed, got %s (%s)", result.Status, result.Output)
	}
	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, context.Canceled) && err != io.ErrClosedPipe {
			t.Logf("server exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}

	// The agent's turn completed — assert what it observed.
	if len(agent.rpcErrs) > 0 {
		t.Fatalf("agent RPC errors: %v", agent.rpcErrs)
	}
	// The handshake must advertise the execution-environment capabilities —
	// an agent only uses client fs/terminal RPCs when they are declared.
	if !agent.clientCaps.Terminal || !agent.clientCaps.FS.ReadTextFile || !agent.clientCaps.FS.WriteTextFile {
		t.Fatalf("client capabilities not declared in handshake: %+v", agent.clientCaps)
	}
	// The workspace MCP server must be advertised at session/new.
	if len(agent.mcpServers) != 1 || agent.mcpServers[0].Type != "http" || !strings.Contains(agent.mcpServers[0].URL, "/mcp/run-1") {
		t.Fatalf("workspace MCP server not advertised at session/new: %+v", agent.mcpServers)
	}
	if agent.readContent != "hello worktree" {
		t.Fatalf("fs read through proxy: want %q, got %q", "hello worktree", agent.readContent)
	}
	if agent.termExit == nil || *agent.termExit != 0 {
		t.Fatalf("terminal exit: want 0, got %+v", agent.termExit)
	}
	if !strings.Contains(agent.termOutput, "run-1") {
		t.Fatalf("terminal output %q missing injected AGENTWORK_RUN_ID", agent.termOutput)
	}
	if len(messages) != 1 || messages[0] != "work done" {
		t.Fatalf("event stream: want [work done], got %v", messages)
	}
}
