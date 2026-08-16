package acpbackend

import (
	"context"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/proto"
)

// ── Persistent ACP session (决策 6-21) ──
//
// One (agent, goal) pair keeps ONE process + ONE ACP session alive across
// its turns: OpenSession does initialize + session/new ONCE; each wake is a
// session/prompt on the same session; cancel interrupts the in-flight prompt
// via session/cancel WITHOUT tearing the session down. The runtime's own
// memory/summary machinery owns context growth.

// session implements proto.Session over the ACP SDK.
type session struct {
	sess      *acp.Session
	conn      proto.Conn
	sessionID string
	cwd       string

	// turnMu serializes wakes: one prompt at a time per session (a concurrent
	// wake — e.g. a consult on the owner while its run is live — waits).
	turnMu sync.Mutex
	// current is the in-flight turn's forwarder (events route here). Nil
	// between turns; events outside a turn are dropped (the runtime emits
	// nothing between prompts).
	curMu   sync.Mutex
	current *eventForwarder
}

// OpenSession opens a persistent session — the SAME Backend the registry
// holds implements both surfaces (Execute for the per-run path, OpenSession
// for the pooled path, 决策 6-21).
func (b *Backend) OpenSession(ctx context.Context, spec proto.SessionSpec) (proto.Session, error) {
	conn := spec.Conn
	sess := acp.NewSession(conn.R, conn.W, conn.Close)

	// Declare the execution-environment capabilities in the handshake (same
	// contract as Execute — an agent only uses the client's fs/terminal RPCs
	// when the client advertises them).
	initReq := acp.InitializeRequest{ProtocolVersion: 1}
	if spec.ClientHandler != nil {
		initReq.ClientCapabilities = acp.ClientCapabilities{
			FS:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		}
	}
	if _, err := sess.Initialize(ctx, initReq); err != nil {
		conn.Close()
		return nil, err
	}
	newResp, err := sess.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        spec.Cwd,
		McpServers: spec.McpServers,
		// The ACP convention for naming the client: _meta._channel lets the
		// agent's runtime identify WHERE a session came from (a multi-tenant
		// openagent can route/group platform sessions by it).
		Meta: map[string]any{"_channel": "agentwork"},
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := &session{sess: sess, conn: conn, sessionID: string(newResp.SessionID), cwd: spec.Cwd}
	// ONE handler for the session's whole life — it routes to the current
	// turn's forwarder.
	sess.SetEventHandler(s)
	if spec.ClientHandler != nil {
		sess.SetClientRequestHandler(spec.ClientHandler)
	}
	return s, nil
}

// LoadSession resumes a prior session by id (session/load, history
// replay) — the multica-style resume pointer carried in the dispatch.
func (s *session) LoadSession(ctx context.Context, priorSessionID string) error {
	_, err := s.sess.LoadSession(ctx, acp.LoadSessionRequest{
		SessionID: priorSessionID,
		Cwd:       s.cwd,
	})
	return err
}

// SessionID returns the ACP session id.
func (s *session) SessionID() string { return s.sessionID }

// Prompt runs one turn. Serialized by turnMu; the events/results channels
// are per-turn and close when the turn ends.
func (s *session) Prompt(ctx context.Context, prompt string) (*proto.Run, error) {
	s.turnMu.Lock()

	events := make(chan proto.Event, 256)
	results := make(chan proto.Result, 1)
	fwd := &eventForwarder{events: events}
	s.curMu.Lock()
	s.current = fwd
	s.curMu.Unlock()
	go fwd.pump()

	// Cancellation (决策 6-21): ctx.Done cancels THIS turn via
	// session/cancel — the session survives. A runtime that ignores the
	// cancel gets its transport broken after a grace: the turn dies with it,
	// and the daemon evicts the dead session (the next wake starts fresh).
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = s.sess.Cancel(cctx, s.sessionID)
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = s.conn.Close() // transport break → the Prompt below returns
			}
		case <-done:
		}
	}()

	go func() {
		defer s.turnMu.Unlock()
		defer close(results)
		defer close(done)
		defer func() {
			s.curMu.Lock()
			if s.current == fwd {
				s.current = nil
			}
			s.curMu.Unlock()
			fwd.close()
		}()

		// A transport break (process death) must surface as a failed/cancelled
		// turn, not a hang: the ACP SDK's read pump closes the session on EOF,
		// and Prompt returns an error — status derives from ctx.Err() exactly
		// like the per-run Execute path.
		if _, err := s.sess.Prompt(ctx, acp.PromptRequest{
			SessionID: s.sessionID,
			Prompt:    []acp.ContentBlock{{Type: "text", Text: prompt}},
		}); err != nil {
			status := proto.StatusFailed
			if ctx.Err() != nil {
				status = proto.StatusCancelled
			}
			// A transport-level failure (process crash / connection drop)
			// means the session is dead. Close the transport FIRST so the
			// subprocess joins (cmd.Wait) and its stderr buffer is complete
			// before AppendStderr reads it — otherwise the race drops the
			// crash reason (a live stdio crash left no stderr in the output).
			// Idempotent: the daemon's eviction calls Close again.
			if status == proto.StatusFailed {
				_ = s.conn.Close()
			}
			results <- proto.Result{Status: status, Output: proto.AppendStderr("prompt: "+err.Error(), s.conn.Stderr), Err: err, SessionID: s.sessionID}
			return
		}
		results <- proto.Result{Status: proto.StatusCompleted, Output: fwd.lastAssistantText(), SessionID: s.sessionID}
	}()

	return &proto.Run{Events: events, Result: results}, nil
}

// Cancel interrupts the in-flight turn via ACP session/cancel. The session
// survives; only the prompt dies (its blocked Prompt call returns and the
// turn finalizes as cancelled).
func (s *session) Cancel(ctx context.Context) error {
	return s.sess.Cancel(ctx, s.sessionID)
}

// Close tears the session down: protocol close + transport close (kills the
// process group on stdio).
func (s *session) Close() error {
	_ = s.sess.Close()
	return s.conn.Close()
}

// OnAgentMessage etc. route ACP events to the CURRENT turn's forwarder.
// Between turns there is no forwarder — events drop (the runtime emits
// nothing outside a prompt).
func (s *session) route() *eventForwarder {
	s.curMu.Lock()
	defer s.curMu.Unlock()
	return s.current
}

func (s *session) OnAgentMessage(text string) {
	if f := s.route(); f != nil {
		f.OnAgentMessage(text)
	}
}
func (s *session) OnAgentThought(text string) {
	if f := s.route(); f != nil {
		f.OnAgentThought(text)
	}
}
func (s *session) OnToolCall(tc acp.ToolCallUpdate) {
	if f := s.route(); f != nil {
		f.OnToolCall(tc)
	}
}
func (s *session) OnUserMessage(text string)                        {}
func (s *session) OnPlan(acp.Plan)                                  {}
func (s *session) OnAvailableCommandsUpdate([]acp.AvailableCommand) {}
func (s *session) OnModeUpdate(acp.SessionModeId)                   {}
func (s *session) OnConfigOptionUpdate([]acp.SessionConfigOption)   {}
func (s *session) OnUsageUpdate(int, int, *acp.Cost)                {}
func (s *session) OnSessionInfo(string, map[string]any)             {}
