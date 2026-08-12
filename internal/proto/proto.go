// Package proto is the protocol layer: a uniform Backend abstraction over
// the different agent wire protocols (ACP, JSONL streams, JSON-RPC). The
// daemon hands a Backend an already-open transport (built by runtime.Open)
// plus the run's prompt; the Backend speaks its protocol, streams Events,
// and delivers one terminal Result. See DESIGN.md
//
// Transports (stdio/ws/tcp) are wired by internal/runtime; protocols
// (acp/jsonl/jsonrpc) are implemented here. Adding a protocol = adding a
// Backend, with no change to the daemon or scheduler.
package proto

import (
	"context"
	"errors"
	"io"

	"github.com/eushing/agentwork/internal/acp"
)

// ErrUnsupportedProvider is returned when no backend is registered for a
// runtime's provider. Lets the daemon surface a clean "unsupported protocol"
// failure rather than a nil-deref.
var ErrUnsupportedProvider = errors.New("proto: unsupported provider")

// Status is the terminal outcome of a run.
type Status string

const (
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusAborted   Status = "aborted"
	StatusCancelled Status = "cancelled"
)

// Conn is an already-open transport connection, produced by internal/runtime.
// A Backend speaks its protocol over R/W, may inspect Stderr for failure
// diagnostics (stdio transport only), and must call Close when done.
type Conn struct {
	R      io.Reader
	W      io.Writer
	Close  func() error
	Stderr io.Reader // nil for non-stdio transports
}

// ExecuteSpec is what a Backend needs to run one turn.
type ExecuteSpec struct {
	Conn   Conn
	Cwd    string // working directory for the run
	Prompt string // the pre-built opening prompt
	// ClientHandler is the run's Agent→Client RPC handler (execution
	// environment proxy, DESIGN.md 决策 4-8). The type comes from the acp
	// package because the handler methods carry acp request types — proto
	// depends on acp for this one type, and backends without Agent→Client
	// RPCs (jsonl/jsonrpc) leave it nil.
	ClientHandler acp.ClientRequestHandler
	// McpServers are the MCP servers advertised at session/new — the run's
	// workspace MCP server (http /mcp/{runID}), so agents that do not
	// delegate tools to client RPCs still reach the workspace through MCP.
	McpServers []acp.McpServer
}

// EventType discriminates one stream event. Mirrors the ACP update shapes but
// kept protocol-neutral so JSONL/JSON-RPC backends populate the same stream.
type EventType string

const (
	EventMessage  EventType = "message"   // an assistant message chunk
	EventThought  EventType = "thought"   // agent reasoning chunk
	EventToolUse  EventType = "tool_use"  // a tool call started
	EventToolResult EventType = "tool_result" // a tool call finished
	EventLog      EventType = "log"
	EventError    EventType = "error"
)

// Event is one streamed item during a turn.
type Event struct {
	Type    EventType `json:"type"`
	Text    string    `json:"text,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	CallID  string    `json:"call_id,omitempty"`
	Input   string    `json:"input,omitempty"`
	Output  string    `json:"output,omitempty"`
}

// Result is the terminal outcome, delivered exactly once on the Run's Result
// channel, which is then closed.
type Result struct {
	Status    Status  `json:"status"`
	Output    string  `json:"output,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Err       error   `json:"-"`
}

// Run is the handle a Backend returns. Events streams items as they arrive
// (closed when the turn ends); Result delivers exactly one terminal outcome
// and is then closed.
type Run struct {
	Events <-chan Event
	Result <-chan Result
}

// Backend speaks one wire protocol. Execute must start its protocol on the
// transport, stream Events, deliver one Result, and close both channels. It
// must respect ctx cancellation (a cancelled run should terminate promptly
// and report StatusCancelled on Result).
type Backend interface {
	Execute(ctx context.Context, spec ExecuteSpec) (*Run, error)
}

// Registry maps a provider name to its Backend. The daemon looks up by
// runtime.provider.
type Registry struct {
	backends map[string]Backend
}

func NewRegistry() *Registry { return &Registry{backends: make(map[string]Backend)} }

// Register attaches a Backend to a provider name (acp|jsonl|jsonrpc|...).
func (r *Registry) Register(provider string, b Backend) {
	r.backends[provider] = b
}

// Get returns the Backend for a provider, or ErrUnsupportedProvider.
func (r *Registry) Get(provider string) (Backend, error) {
	b, ok := r.backends[provider]
	if !ok {
		return nil, ErrUnsupportedProvider
	}
	return b, nil
}