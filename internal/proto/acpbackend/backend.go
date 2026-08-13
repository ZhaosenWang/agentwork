// Package acpbackend adapts internal/acp (ACP v1 over JSON-RPC 2.0) to the
// proto.Backend interface. It is the only concrete backend wired for MVP; the
// JSONL and JSON-RPC backends are stubbed (see jsonlbackend/jsonrpcbackend).
package acpbackend

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/proto"
)

// Backend is the ACP provider backend. It builds an acp.Session over the
// transport Conn, runs the ACP handshake, sends one Prompt, drains the
// session/update stream into proto.Events, and delivers a terminal Result.
type Backend struct{}

// New returns the ACP backend.
func New() *Backend { return &Backend{} }

func (b *Backend) Execute(ctx context.Context, spec proto.ExecuteSpec) (*proto.Run, error) {
	conn := spec.Conn
	sess := acp.NewSession(conn.R, conn.W, conn.Close)

	events := make(chan proto.Event, 256)
	results := make(chan proto.Result, 1)

	// Context cancellation must BREAK a blocking read: the agent process can
	// hang on a network call with no output, and the idle/maxRunDuration
	// watchdog's ctx cancellation would otherwise never interrupt the read
	// blocked on the transport — the run would sit 'running' forever (a live
	// 10-hour hang: opencode stuck on a model API request, watchdog fired,
	// read never returned). Closing the transport makes the read return EOF →
	// Prompt errors → the drain reports cancelled (ctx.Err() != nil).
	//
	// The waiter watches `done` (closed when the drain exits), NOT `results`:
	// consuming a value from the buffered results channel would STEAL the
	// drain's single result and the daemon would see "backend closed result
	// channel" (a failed run for a completed turn — the regression this
	// waiter caused).
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
			// Turn finished normally — nothing to break.
		}
	}()

	// One drain goroutine drives the turn to completion. It runs the ACP
	// handshake, the Prompt, and then closes both channels with a Result.
	// The eventForwarder's pump goroutine closes `events` (after draining its
	// queue); `results` closes here.
	go func() {
		defer close(results)
		defer close(done)

		// On any failure path, surface the agent's captured stderr (stdio
		// transport) so a bad-args / missing-config agent isn't reported as a
		// bare "transport closed".

		// Declare the execution-environment capabilities in the handshake:
		// an agent only uses the client's fs/terminal RPCs when the client
		// advertises them (initialize clientCapabilities). Without this the
		// agent silently falls back to its own tools in its own environment
		// — a live remote run proved it (the agent listed its own
		// shell/read/write and reported no client tools).
		initReq := acp.InitializeRequest{ProtocolVersion: 1}
		if spec.ClientHandler != nil {
			initReq.ClientCapabilities = acp.ClientCapabilities{
				FS:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
				Terminal: true,
			}
		}
		if _, err := sess.Initialize(ctx, initReq); err != nil {
			// The pump (the ONLY closer of `events`) starts after NewSession;
			// these pre-session failures must close it themselves or the
			// daemon's `for ev := range run.Events` blocks forever and the
			// run never finishes (a live zombie: an agent that stalled
			// connecting to the advertised MCP server left its run
			// 'running' indefinitely after the idle watchdog fired).
			close(events)
			results <- proto.Result{Status: proto.StatusFailed, Output: proto.AppendStderr("initialize: "+err.Error(), conn.Stderr), Err: err}
			return
		}
		newResp, err := sess.NewSession(ctx, acp.NewSessionRequest{Cwd: spec.Cwd, McpServers: spec.McpServers})
		if err != nil {
			close(events)
			results <- proto.Result{Status: proto.StatusFailed, Output: proto.AppendStderr("new session: "+err.Error(), conn.Stderr), Err: err}
			return
		}
		fwd := &eventForwarder{events: events}
		sess.SetEventHandler(fwd)
		// Execution-environment proxy (DESIGN.md 决策 4-8): the run's
		// handler answers the agent's fs/terminal RPCs.
		if spec.ClientHandler != nil {
			sess.SetClientRequestHandler(spec.ClientHandler)
		}
		go fwd.pump()

		if _, err := sess.Prompt(ctx, acp.PromptRequest{
			SessionID: newResp.SessionID,
			Prompt:    []acp.ContentBlock{{Type: "text", Text: spec.Prompt}},
		}); err != nil {
			// ctx cancellation → treat as cancelled rather than failed so the
			// daemon/idle-watchdog can distinguish "we stopped it" from "it broke".
			status := proto.StatusFailed
			if ctx.Err() != nil {
				status = proto.StatusCancelled
			}
			results <- proto.Result{Status: status, Output: proto.AppendStderr("prompt: "+err.Error(), conn.Stderr), Err: err, SessionID: string(newResp.SessionID)}
			return
		}
		// Carry the assistant's text (the final answer) as Result.Output so the
		// goal layer can fold it into a child-summary / run-detail without
		// requiring the daemon to replay the event stream. Collected by the
		// eventForwarder during the turn.
		results <- proto.Result{Status: proto.StatusCompleted, Output: fwd.lastAssistantText(), SessionID: string(newResp.SessionID)}
		fwd.close()
	}()

	return &proto.Run{Events: events, Result: results}, nil
}

// eventForwarder adapts acp.EventHandler → proto.Event channel. It also
// accumulates the assistant's message text so the Result can carry the final
// answer as Output (for a child-summary / run-detail) without replaying the
// stream. Accumulation is append-only; lastAssistantText returns the full text
// once the turn is done. Guarded because the drain reader goroutine invokes the
// callbacks concurrently with the Execute goroutine reading the result.
//
// Delivery: ACP callbacks push into a mutex-guarded queue (non-blocking,
// never drops), and a pump goroutine forwards the queue to the events
// channel (blocking send — the daemon consumes). The pump is the ONLY
// closer of `events`: it closes once the forwarder is closed AND the queue
// is drained, so a late ACP callback can never send on a closed channel
// (the old select-default push silently dropped events AND raced the
// close — a real panic window).
type eventForwarder struct {
	events  chan<- proto.Event
	mu      sync.Mutex
	queue   []proto.Event
	closed  bool
	msg     strings.Builder
	truncate int
}

const assistantOutputCap = 8 * 1024 // keep Result.Output from growing unbounded

// push enqueues an event. Non-blocking and lossless (bounded by memory, not
// by the consumer's speed). After close, pushes are dropped.
func (f *eventForwarder) push(e proto.Event) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.queue = append(f.queue, e)
	f.mu.Unlock()
}

// close marks the forwarder closed; the pump drains the remaining queue and
// then closes the events channel.
func (f *eventForwarder) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// pump forwards queued events to the events channel until closed-and-empty,
// then closes it. Started once per Execute.
func (f *eventForwarder) pump() {
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		f.mu.Lock()
		if len(f.queue) == 0 {
			if f.closed {
				f.mu.Unlock()
				close(f.events)
				return
			}
			f.mu.Unlock()
			<-ticker.C
			continue
		}
		e := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		f.events <- e
	}
}

func (f *eventForwarder) lastAssistantText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.TrimSpace(f.msg.String())
}

func (f *eventForwarder) OnAgentMessage(text string) {
	f.push(proto.Event{Type: proto.EventMessage, Text: text})
	f.mu.Lock()
	if f.truncate >= 0 {
		if f.truncate+len(text) > assistantOutputCap {
			// Cap reached: stop accumulating to bound memory.
			f.truncate = -1
		} else {
			f.msg.WriteString(text)
			f.truncate += len(text)
		}
	}
	f.mu.Unlock()
}
func (f *eventForwarder) OnAgentThought(text string) {
	f.push(proto.Event{Type: proto.EventThought, Text: text})
}
func (f *eventForwarder) OnUserMessage(text string) {} // history replay only
func (f *eventForwarder) OnToolCall(tc acp.ToolCallUpdate) {
	// A tool call with a result → tool_result; a started one → tool_use.
	ev := proto.Event{
		Type:   proto.EventToolUse,
		Tool:   tc.Title,
		CallID: tc.ToolCallID,
		Input:  toJSONString(tc.RawInput),
		Output: toJSONString(tc.RawOutput),
	}
	if tc.Status == "completed" || tc.Status == "failed" {
		ev.Type = proto.EventToolResult
	}
	f.push(ev)
}

// toJSONString renders an arbitrary value as a JSON string (empty on nil/error).
func toJSONString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
func (f *eventForwarder) OnPlan(acp.Plan)                                 {}
func (f *eventForwarder) OnAvailableCommandsUpdate([]acp.AvailableCommand) {}
func (f *eventForwarder) OnModeUpdate(acp.SessionModeId)                   {}
func (f *eventForwarder) OnConfigOptionUpdate([]acp.SessionConfigOption)    {}
func (f *eventForwarder) OnUsageUpdate(int, int, *acp.Cost)                 {}
func (f *eventForwarder) OnSessionInfo(string, map[string]any)              {}