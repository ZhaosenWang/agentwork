package acpbackend

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/proto"
)

// ── Session backend (决策 6-21): one ACP session, many prompts ──

// fakeAgent is a minimal in-memory ACP agent: it answers each prompt with a
// canned message and records what it was asked.
type fakeAgent struct {
	mu        sync.Mutex
	prompts   []string
	sessionID acp.SessionId
	// hold, when set, blocks OnPrompt until it closes — lets a test cancel
	// the turn BEFORE the fake can respond (deterministic cancel ordering
	// instead of racing an immediate response, which flaked under full-suite
	// load).
	hold chan struct{}
}

func (f *fakeAgent) OnInitialize(ctx context.Context, req acp.InitializeRequest) (*acp.InitializeResponse, error) {
	return &acp.InitializeResponse{ProtocolVersion: 1, AgentCapabilities: acp.AgentCapabilities{LoadSession: true}}, nil
}
func (f *fakeAgent) OnNewSession(ctx context.Context, req acp.NewSessionRequest) (*acp.NewSessionResponse, error) {
	f.sessionID = "sess-1"
	return &acp.NewSessionResponse{SessionID: f.sessionID}, nil
}
func (f *fakeAgent) OnLoadSession(ctx context.Context, req acp.LoadSessionRequest, s acp.SessionEventSender) (*acp.LoadSessionResponse, error) {
	return &acp.LoadSessionResponse{}, nil
}
func (f *fakeAgent) OnResumeSession(ctx context.Context, req acp.ResumeSessionRequest) (*acp.ResumeSessionResponse, error) {
	return &acp.ResumeSessionResponse{}, nil
}
func (f *fakeAgent) OnCloseSession(ctx context.Context, req acp.CloseSessionRequest) (*acp.CloseSessionResponse, error) {
	return &acp.CloseSessionResponse{}, nil
}
func (f *fakeAgent) OnDeleteSession(ctx context.Context, req acp.DeleteSessionRequest) (*acp.DeleteSessionResponse, error) {
	return &acp.DeleteSessionResponse{}, nil
}
func (f *fakeAgent) OnListSessions(ctx context.Context, req acp.ListSessionsRequest) (*acp.ListSessionsResponse, error) {
	return &acp.ListSessionsResponse{}, nil
}
func (f *fakeAgent) OnSetSessionMode(ctx context.Context, req acp.SetSessionModeRequest) (*acp.SetSessionModeResponse, error) {
	return &acp.SetSessionModeResponse{}, nil
}
func (f *fakeAgent) OnSetSessionConfigOption(ctx context.Context, req acp.SetSessionConfigOptionRequest) (*acp.SetSessionConfigOptionResponse, error) {
	return &acp.SetSessionConfigOptionResponse{}, nil
}
func (f *fakeAgent) OnPrompt(ctx context.Context, req acp.PromptRequest, s acp.SessionEventSender) (*acp.PromptResponse, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, textOf(req.Prompt))
	f.mu.Unlock()
	if f.hold != nil {
		<-f.hold
	}
	if err := s.SendAgentMessage("answer: " + textOf(req.Prompt)); err != nil {
		return nil, err
	}
	return &acp.PromptResponse{StopReason: "end_turn"}, nil
}
func (f *fakeAgent) OnLogout(ctx context.Context, req acp.LogoutRequest) (*acp.LogoutResponse, error) {
	return &acp.LogoutResponse{}, nil
}
func (f *fakeAgent) OnCancel(ctx context.Context, sid acp.SessionId) error { return nil }
func (f *fakeAgent) OnAuthenticate(ctx context.Context, req acp.AuthenticateRequest) (*acp.AuthenticateResponse, error) {
	return &acp.AuthenticateResponse{}, nil
}

func textOf(blocks []acp.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}

// openTestSession wires the backend against the in-memory agent over pipes.
func openTestSession(t *testing.T, f *fakeAgent) proto.Session {
	t.Helper()
	cr, sw := io.Pipe() // client reads ← server writes
	sr, cw := io.Pipe() // server reads ← client writes
	srv := acp.NewServer("fake-agent", "1", f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.RunTransport(ctx, sw, sr)
	}()
	t.Cleanup(func() { cancel(); _ = cw.Close(); _ = cr.Close(); <-done })
	conn := proto.Conn{R: cr, W: cw, Close: func() error { cancel(); return nil }}
	s, err := New().OpenSession(context.Background(), proto.SessionSpec{Conn: conn, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	return s
}

// TestSessionBackendMultiPrompt: ONE session serves MANY prompts — the core
// of 决策 6-21 (no per-run process, no per-run session/new).
func TestSessionBackendMultiPrompt(t *testing.T) {
	f := &fakeAgent{}
	s := openTestSession(t, f)
	ctx := context.Background()

	run1, err := s.Prompt(ctx, "first wake")
	if err != nil {
		t.Fatal(err)
	}
	res1 := <-run1.Result
	if res1.Status != proto.StatusCompleted {
		t.Fatalf("first wake: %v (err %v)", res1.Status, res1.Err)
	}
	if !strings.Contains(res1.Output, "first wake") {
		t.Fatalf("first wake output: %q", res1.Output)
	}

	run2, err := s.Prompt(ctx, "second wake")
	if err != nil {
		t.Fatal(err)
	}
	res2 := <-run2.Result
	if res2.Status != proto.StatusCompleted || !strings.Contains(res2.Output, "second wake") {
		t.Fatalf("second wake: %v %q (err %v)", res2.Status, res2.Output, res2.Err)
	}

	f.mu.Lock()
	n := len(f.prompts)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("the fake agent must see BOTH prompts on one session, got %d", n)
	}
}

// TestSessionBackendCancelKeepsSession: cancel interrupts the in-flight
// prompt; the SESSION survives and serves the next wake (决策 6-21 cancel
// semantics — a cancelled run is not a dead session).
func TestSessionBackendCancelKeepsSession(t *testing.T) {
	// The fake holds its response until the test releases it — the cancel
	// lands BEFORE the fake can complete, so the cancelled outcome is
	// deterministic (an immediately-responding fake raced the cancel and
	// flaked under full-suite load).
	hold := make(chan struct{})
	f := &fakeAgent{hold: hold}
	s := openTestSession(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	run1, err := s.Prompt(ctx, "will be cancelled")
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the platform cancels this wake
	res1 := <-run1.Result
	close(hold) // release the fake only AFTER the verdict — its late response can never beat the cancel
	if res1.Status != proto.StatusCancelled {
		t.Fatalf("a cancelled wake reports cancelled, got %v (err %v)", res1.Status, res1.Err)
	}

	// The SESSION is alive — the next wake completes normally.
	run2, err := s.Prompt(context.Background(), "after cancel")
	if err != nil {
		t.Fatal(err)
	}
	res2 := <-run2.Result
	if res2.Status != proto.StatusCompleted {
		t.Fatalf("the session must survive a cancelled wake, got %v (err %v)", res2.Status, res2.Err)
	}
}

// TestSessionBackendSerializesWakes: two concurrent prompts serialize — the
// second completes after the first (one session = one in-flight turn).
func TestSessionBackendSerializesWakes(t *testing.T) {
	f := &fakeAgent{}
	s := openTestSession(t, f)
	ctx := context.Background()

	run1, err := s.Prompt(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	done1 := make(chan struct{})
	go func() { <-run1.Result; close(done1) }()

	start := time.Now()
	run2, err := s.Prompt(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	res2 := <-run2.Result
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("wake one must have finished first")
	}
	if res2.Status != proto.StatusCompleted {
		t.Fatalf("serialized second wake: %v", res2.Status)
	}
	_ = time.Since(start)
}

// TestSessionTransportCloseSurfacesError: a transport-level failure (agent
// process crash / connection drop) during a prompt must:
//  1. Surface the read error in the run output (not just "transport closed").
//  2. Call conn.Close before reading stderr (so stdio stderr is complete).
func TestSessionTransportCloseSurfacesError(t *testing.T) {
	cr, sw := io.Pipe() // client reads ← mini agent writes
	sr, cw := io.Pipe() // mini agent reads ← client writes

	var closeCalled atomic.Bool
	conn := proto.Conn{
		R: cr, W: cw,
		Close: func() error { closeCalled.Store(true); return nil },
	}

	// Mini agent: respond to initialize + session/new, then on session/prompt
	// close the write pipe with an error to simulate a transport drop.
	go func() {
		defer cw.Close()
		sc := bufio.NewScanner(sr)
		sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for sc.Scan() {
			var msg struct {
				ID     json.RawMessage `json:"id,omitempty"`
				Method string          `json:"method,omitempty"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil {
				continue
			}
			switch msg.Method {
			case "initialize":
				line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}`, msg.ID)
				_, _ = sw.Write([]byte(line + "\n"))
			case "session/new":
				line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"sess-1"}}`, msg.ID)
				_, _ = sw.Write([]byte(line + "\n"))
			case "session/prompt":
				_ = sw.CloseWithError(errors.New("connection reset by peer"))
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cr.Close()
		_ = sr.Close()
	})

	s, err := New().OpenSession(context.Background(), proto.SessionSpec{Conn: conn, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	run, err := s.Prompt(context.Background(), "crash test")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	res := <-run.Result

	if res.Status != proto.StatusFailed {
		t.Fatalf("expected failed, got %v (err %v)", res.Status, res.Err)
	}
	if !strings.Contains(res.Output, "transport closed") {
		t.Fatalf("output should mention transport closed, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "connection reset by peer") {
		t.Fatalf("output should contain the read error, got: %s", res.Output)
	}
	if !closeCalled.Load() {
		t.Fatalf("conn.Close should be called on transport failure")
	}
}
