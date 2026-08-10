// Package acpbackend adapts internal/acp (ACP v1 over JSON-RPC 2.0) to the
// proto.Backend interface. It is the only concrete backend wired for MVP; the
// JSONL and JSON-RPC backends are stubbed (see jsonlbackend/jsonrpcbackend).
package acpbackend

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"

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

	// One drain goroutine drives the turn to completion. It runs the ACP
	// handshake, the Prompt, and then closes both channels with a Result.
	go func() {
		defer close(events)
		defer close(results)

		// On any failure path, surface the agent's captured stderr (stdio
		// transport) so a bad-args / missing-config agent isn't reported as a
		// bare "transport closed".
		withStderr := func(summary string) string {
			if conn.Stderr != nil {
				if b, err := io.ReadAll(conn.Stderr); err == nil && len(b) > 0 {
					return summary + "\nstderr: " + string(b)
				}
			}
			return summary
		}

		if _, err := sess.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1}); err != nil {
			results <- proto.Result{Status: proto.StatusFailed, Output: withStderr("initialize: " + err.Error()), Err: err}
			return
		}
		newResp, err := sess.NewSession(ctx, acp.NewSessionRequest{
			Cwd:        spec.Cwd,
			McpServers: []acp.McpServer{}, // non-nil so JSON marshals as [] not null
		})
		if err != nil {
			results <- proto.Result{Status: proto.StatusFailed, Output: withStderr("new session: " + err.Error()), Err: err}
			return
		}
		fwd := &eventForwarder{events: events}
		sess.SetEventHandler(fwd)

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
			results <- proto.Result{Status: status, Output: withStderr("prompt: " + err.Error()), Err: err, SessionID: string(newResp.SessionID)}
			return
		}
		// Carry the assistant's text (the final answer) as Result.Output so the
		// goal layer can fold it into a child-summary / run-detail without
		// requiring the daemon to replay the event stream. Collected by the
		// eventForwarder during the turn.
		results <- proto.Result{Status: proto.StatusCompleted, Output: fwd.lastAssistantText(), SessionID: string(newResp.SessionID)}
	}()

	return &proto.Run{Events: events, Result: results}, nil
}

// eventForwarder adapts acp.EventHandler → proto.Event channel. It also
// accumulates the assistant's message text so the Result can carry the final
// answer as Output (for a child-summary / run-detail) without replaying the
// stream. Accumulation is append-only; lastAssistantText returns the full text
// once the turn is done. Guarded because the drain reader goroutine invokes the
// callbacks concurrently with the Execute goroutine reading the result.
type eventForwarder struct {
	events   chan<- proto.Event
	mu       sync.Mutex
	msg      strings.Builder
	truncate int
}

const assistantOutputCap = 8 * 1024 // keep Result.Output from growing unbounded

func (f *eventForwarder) push(e proto.Event) {
	select {
	case f.events <- e:
	default:
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

var _ = json.Marshal