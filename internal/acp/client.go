// ── Client ──
//
// Client speaks the ACP v1 protocol over any transport that provides a
// read/write pair. Methods map 1:1 to the ACP v1 protocol:
//
//	https://agentclientprotocol.com/protocol/v1/schema
//
// The protocol layer ([Session]) is transport-agnostic: it only needs an
// [io.Reader] (agent→client messages) and an [io.Writer] (client→agent
// messages). Transports (stdio subprocess, WebSocket, TCP) are wired up
// by the caller — typically [internal/runtime.Launch] — and handed to
// [NewSession]. ConnectStdio remains as a convenience for the common
// stdio-subprocess case.
//
// Usage:
//
//	// stdio (convenience)
//	client := acp.NewClient("my-app", "1.0.0")
//	sess, _ := client.ConnectStdio(ctx, "my-agent", "-v")
//	defer sess.Close()
//
//	// any transport
//	sess := acp.NewSession(r, w, closeFn)
//	defer sess.Close()
//
//	sess.Initialize(ctx, acp.InitializeRequest{...})
//	sess.NewSession(ctx, acp.NewSessionRequest{...})
//	sess.SetEventHandler(handler)         // receive streaming notifications
//	sess.Prompt(ctx, acp.PromptRequest{...})
//
// The EventHandler receives session/update notifications (agent messages,
// tool calls, plan updates, config changes, etc.) from the agent in real
// time as they arrive. Notifications are dispatched asynchronously by a
// background reader goroutine started automatically by NewSession.
//
// See [Session] for the full method set and [EventHandler] for notification
// callbacks.

package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"sync"
)

// Client is a handle carrying the implementation identity for ACP requests.
// It is transport-agnostic; ConnectStdio is kept only as a convenience for
// the stdio-subprocess transport.
type Client struct {
	name    string
	version string
}

// NewClient creates an ACP [Client] with the given implementation identity.
func NewClient(name, version string) *Client {
	return &Client{name: name, version: version}
}

// ConnectStdio spawns an ACP agent subprocess and communicates over
// stdin/stdout with newline-delimited JSON (JSON-RPC 2.0).
//
// command is the path to the executable; args are its CLI arguments.
// env is the full environment for the subprocess (os.Environ()-style
// "KEY=VALUE" strings); pass nil to inherit the parent's environment.
// The background reader goroutine starts automatically and dispatches
// session/update notifications to the [EventHandler].
//
// Call [Session.Close] to kill the process and release resources.
//
// For non-stdio transports (WebSocket, TCP), build the read/write pair
// yourself and construct the session with [NewSession] instead.
func (c *Client) ConnectStdio(ctx context.Context, env []string, command string, args ...string) (*Session, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp start %q: %w", command, err)
	}

	closeFn := func() error {
		if wc, ok := stdin.(io.WriteCloser); ok {
			_ = wc.Close()
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return nil
	}
	return NewSession(stdout, stdin, closeFn, WithStderr(&stderrBuf)), nil
}

// ── Session ──

// Session is an active ACP conversation over a transport. It is
// transport-agnostic: it reads JSON-RPC 2.0 messages from r and writes
// them to w, and calls closeFn on Close. The transport (stdio subprocess,
// WebSocket, TCP, …) is wired up by the caller before constructing the
// session.
//
// Public methods map 1:1 to ACP v1 protocol methods — each sends a
// JSON-RPC 2.0 request and blocks until the response arrives.
//
// Streaming notifications from the agent (agent messages, tool calls,
// plans, config changes, etc.) are delivered to the [EventHandler] set
// via [Session.SetEventHandler]. The handler must be set before
// [Session.Prompt] to receive streaming output.
type Session struct {
	r       io.Reader    // agent→client (responses + notifications)
	w       io.Writer    // client→agent (requests + responses to agent RPC)
	closeFn func() error // transport cleanup; called by Close

	stderrBuf *bytes.Buffer // optional: captured stderr (stdio transport only)

	writeMu *sync.Mutex

	mu         sync.Mutex
	sessionID  string
	nextID     int64
	pending    map[string]*pendingCall
	eh         EventHandler
	clientReqH ClientRequestHandler
}

type pendingCall struct {
	resp rpcResponse
	done chan struct{}
}

// sessionOption configures a Session at construction.
type sessionOption func(*Session)

// WithStderr attaches a stderr capture buffer to the session. Only
// meaningful for the stdio transport; other transports have no stderr.
func WithStderr(buf *bytes.Buffer) sessionOption {
	return func(s *Session) { s.stderrBuf = buf }
}

// NewSession constructs a transport-agnostic ACP session over the given
// read/write pair. closeFn is called by [Session.Close] and must tear down
// the transport (kill the subprocess, close the WebSocket, etc.). A
// background reader goroutine starts automatically and dispatches
// notifications to the [EventHandler] once set.
//
// r is expected to deliver newline-delimited JSON (JSON-RPC 2.0). The
// reader goroutine buffers up to 10 MiB per line.
func NewSession(r io.Reader, w io.Writer, closeFn func() error, opts ...sessionOption) *Session {
	s := &Session{
		r:       r,
		w:       w,
		closeFn: closeFn,
		writeMu: new(sync.Mutex),
		pending: make(map[string]*pendingCall),
	}
	for _, o := range opts {
		o(s)
	}
	go s.startReader()
	return s
}

// ── ACP Protocol Methods ──
//
// Each method sends the corresponding ACP v1 JSON-RPC request and blocks
// until the agent's response arrives. Protocols docs:
//
//	https://agentclientprotocol.com/protocol/v1/schema

// Initialize performs the ACP handshake: initialize.
// Must be called once after ConnectStdio before any session methods.
func (s *Session) Initialize(ctx context.Context, req InitializeRequest) (*InitializeResponse, error) {
	var resp InitializeResponse
	if err := s.call(ctx, "initialize", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// NewSession creates a new session: session/new.
// The returned session ID is stored internally and reused for subsequent
// [Session.Prompt], [Session.LoadSession], [Session.ResumeSession], and
// [Session.CloseSession] calls.
func (s *Session) NewSession(ctx context.Context, req NewSessionRequest) (*NewSessionResponse, error) {
	var resp NewSessionResponse
	if err := s.call(ctx, "session/new", req, &resp); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessionID = resp.SessionID
	s.mu.Unlock()
	return &resp, nil
}

// normalizeNilSlices deep-converts nil slices into empty arrays before JSON
// marshaling. ACP servers differ in strictness: openagent tolerates null
// arrays, opencode's zod schema REJECTS them ("expected array, received
// undefined") — the same request (e.g. session/new's mcpServers) must be
// accepted by both. omitempty fields are still omitted when zero.
func normalizeNilSlices(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return v
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return v
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return []any{}
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = normalizeNilSlices(rv.Index(i).Interface())
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		t := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Field(i)
			if !f.CanInterface() {
				continue
			}
			tag := t.Field(i).Tag.Get("json")
			parts := strings.Split(tag, ",")
			name := parts[0]
			if name == "" || name == "-" {
				continue
			}
			omitEmpty := false
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitEmpty = true
				}
			}
			if omitEmpty && f.IsZero() {
				// A nil slice with omitempty is still emitted as an empty
				// array: opencode's zod schema rejects MISSING arrays
				// ("expected array, received undefined") — e.g.
				// session/new mcpServers[].headers must be present even
				// when empty. An empty array is accepted by strict and
				// lenient servers alike.
				if f.Kind() == reflect.Slice {
					out[name] = []any{}
					continue
				}
				continue
			}
			out[name] = normalizeNilSlices(f.Interface())
		}
		return out
	default:
		return v
	}
}

// SetEventHandler registers the callback that receives session/update
// notifications from the agent. Must be set before [Session.Prompt] to
// capture streaming output.
func (s *Session) SetEventHandler(h EventHandler) {
	s.mu.Lock()
	s.eh = h
	s.mu.Unlock()
}

// SetClientRequestHandler registers the handler that responds to Agent→Client
// RPC requests (request_permission, fs/read_text_file, terminal/*, etc.).
// If not set, unrecognised agent→client requests return MethodNotFound.
func (s *Session) SetClientRequestHandler(h ClientRequestHandler) {
	s.mu.Lock()
	s.clientReqH = h
	s.mu.Unlock()
}

// Prompt sends a user prompt and blocks until the turn completes:
// session/prompt.
//
// Streaming output (agent messages, tool calls, plan updates, etc.) is
// delivered to the registered [EventHandler] concurrently via the
// background reader goroutine.
func (s *Session) Prompt(ctx context.Context, req PromptRequest) (*PromptResponse, error) {
	s.mu.Lock()
	req.SessionID = s.sessionID
	s.mu.Unlock()
	var resp PromptResponse
	if err := s.call(ctx, "session/prompt", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// LoadSession loads an existing session with history replay: session/load.
// The agent replays conversation history via session/update notifications
// before responding.
func (s *Session) LoadSession(ctx context.Context, req LoadSessionRequest) (*LoadSessionResponse, error) {
	var resp LoadSessionResponse
	if err := s.call(ctx, "session/load", req, &resp); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessionID = req.SessionID
	s.mu.Unlock()
	return &resp, nil
}

// ResumeSession resumes an existing session without history replay:
// session/resume.
func (s *Session) ResumeSession(ctx context.Context, req ResumeSessionRequest) (*ResumeSessionResponse, error) {
	var resp ResumeSessionResponse
	if err := s.call(ctx, "session/resume", req, &resp); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessionID = req.SessionID
	s.mu.Unlock()
	return &resp, nil
}

// CloseSession closes the active session: session/close.
func (s *Session) CloseSession(ctx context.Context) error {
	s.mu.Lock()
	sid := s.sessionID
	s.mu.Unlock()
	if sid == "" {
		return nil
	}
	_, err := s.request(ctx, "session/close", CloseSessionRequest{SessionID: sid})
	return err
}

// DeleteSession permanently removes a session and all its data:
// session/delete.
func (s *Session) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.request(ctx, "session/delete", DeleteSessionRequest{SessionID: sessionID})
	return err
}

// ListSessions returns the sessions available on the agent: session/list.
func (s *Session) ListSessions(ctx context.Context, req ListSessionsRequest) (*ListSessionsResponse, error) {
	var resp ListSessionsResponse
	if err := s.call(ctx, "session/list", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetSessionMode changes the active session mode: session/set_mode.
func (s *Session) SetSessionMode(ctx context.Context, req SetSessionModeRequest) (*SetSessionModeResponse, error) {
	var resp SetSessionModeResponse
	if err := s.call(ctx, "session/set_mode", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetSessionConfigOption changes a config option: session/set_config_option.
// The agent's response contains the full configOptions array (not a delta).
func (s *Session) SetSessionConfigOption(ctx context.Context, req SetSessionConfigOptionRequest) (*SetSessionConfigOptionResponse, error) {
	var resp SetSessionConfigOptionResponse
	if err := s.call(ctx, "session/set_config_option", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Cancel sends a session/cancel notification to cancel an ongoing prompt.
// This is a notification (no response expected).
func (s *Session) Cancel(ctx context.Context, sessionID string) error {
	return s.notify(ctx, "session/cancel", CancelNotification{SessionID: sessionID})
}

// Logout ends the current authenticated state. Must only be called when the
// Agent advertised auth.logout capability.
func (s *Session) Logout(ctx context.Context) (*LogoutResponse, error) {
	var resp LogoutResponse
	if err := s.call(ctx, "logout", LogoutRequest{}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Close tears down the transport via the closeFn provided at construction.
// Call when the client is done with the connection.
func (s *Session) Close() error {
	if s.closeFn != nil {
		return s.closeFn()
	}
	return nil
}

// Stderr returns the captured stderr output of the agent subprocess.
func (s *Session) Stderr() string {
	if s.stderrBuf == nil {
		return ""
	}
	return s.stderrBuf.String()
}

// ── JSON-RPC 2.0 transport ──

// nextReqID returns an auto-incrementing request id.
func (s *Session) nextReqID() string {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	return fmt.Sprintf("%d", id)
}

// call sends a JSON-RPC 2.0 request and unmarshals the result.
func (s *Session) call(ctx context.Context, method string, params, result any) error {
	resp, err := s.request(ctx, method, params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("acp: %s: %s", method, resp.Error.Message)
	}
	return json.Unmarshal(resp.Result, result)
}

// request sends a JSON-RPC 2.0 request and returns the full response.
// It registers a pending call, writes the request to stdin, and blocks
// until the reader goroutine signals completion or ctx is cancelled.
func (s *Session) request(ctx context.Context, method string, params any) (rpcResponse, error) {
	id := s.nextReqID()
	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: normalizeNilSlices(params)}

	reqBody, _ := json.Marshal(req)
	call := &pendingCall{done: make(chan struct{})}

	s.mu.Lock()
	s.pending[id] = call
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	s.writeMu.Lock()
	_, err := s.w.Write(append(reqBody, '\n'))
	s.writeMu.Unlock()
	if err != nil {
		return rpcResponse{}, fmt.Errorf("acp write %s: %w", method, err)
	}

	select {
	case <-call.done:
	case <-ctx.Done():
		return rpcResponse{}, ctx.Err()
	}
	return call.resp, nil
}

// notify sends a JSON-RPC 2.0 notification (no id field, no response).
func (s *Session) notify(ctx context.Context, method string, params any) error {
	notif := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	body, _ := json.Marshal(notif)
	s.writeMu.Lock()
	_, err := s.w.Write(append(body, '\n'))
	s.writeMu.Unlock()
	return err
}

// ── EventHandler ──

// EventHandler receives session/update notifications from the agent.
// Each callback maps to a sessionUpdate discriminator value:
//
//	https://agentclientprotocol.com/protocol/v1/schema#session%2Fupdate
type EventHandler interface {
	// OnAgentMessage — sessionUpdate "agent_message_chunk".
	OnAgentMessage(text string)
	// OnAgentThought — sessionUpdate "agent_thought_chunk".
	OnAgentThought(text string)
	// OnUserMessage — sessionUpdate "user_message_chunk" (during session/load history replay).
	OnUserMessage(text string)
	// OnToolCall — sessionUpdate "tool_call" / "tool_call_update".
	OnToolCall(tc ToolCallUpdate)
	// OnPlan — sessionUpdate "plan".
	OnPlan(plan Plan)
	// OnAvailableCommandsUpdate — sessionUpdate "available_commands_update".
	OnAvailableCommandsUpdate(cmds []AvailableCommand)
	// OnModeUpdate — sessionUpdate "current_mode_update".
	OnModeUpdate(modeID SessionModeId)
	// OnConfigOptionUpdate — sessionUpdate "config_option_update".
	OnConfigOptionUpdate(opts []SessionConfigOption)
	// OnUsageUpdate — sessionUpdate "usage_update".
	OnUsageUpdate(used, total int, cost *Cost)
	// OnSessionInfo — sessionUpdate "session_info_update".
	OnSessionInfo(title string, metadata map[string]any)
}

// ── Reader goroutine ──

// startReader reads JSON-RPC messages from the transport's read side. Three
// message shapes per JSON-RPC 2.0:
//
//	Request:      method + id present → agent→client RPC. Handle synchronously.
//	Notification: method present, no id → dispatch to EventHandler.
//	Response:     no method, id present → deliver to pending call.
func (s *Session) startReader() {
	sc := bufio.NewScanner(s.r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var env struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id,omitempty"`
			Method  string          `json:"method,omitempty"`
			Params  json.RawMessage `json:"params,omitempty"`
			Result  json.RawMessage `json:"result,omitempty"`
			Error   *Error          `json:"error,omitempty"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}

		switch {
		case env.Method != "" && len(env.ID) > 0:
			// Agent→Client RPC request. Handle synchronously and write
			// the response back to the agent on stdin.
			s.handleAgentRequest(env.Method, env.ID, env.Params)

		case env.Method != "":
			// Notification — dispatch to EventHandler.
			s.dispatchNotif(env.Method, env.Params)

		case len(env.ID) > 0:
			// Response — deliver to pending call.
			callKey := idString(env.ID)
			s.mu.Lock()
			call := s.pending[callKey]
			s.mu.Unlock()
			if call != nil {
				if env.Error != nil {
					call.resp.Error = env.Error
				} else {
					call.resp.Result = env.Result
				}
				close(call.done)
			}
		}
	}

	// The transport closed (EOF or read error). Fail any in-flight calls so
	// blocked Initialize/Prompt/… callers return immediately instead of
	// hanging forever. Without this, a subprocess that exits before replying
	// (e.g. bad args) leaves the task stuck in "running" until the idle
	// watchdog fires minutes later.
	//
	// Surface the scanner's read error (if any) so a ws/tcp connection drop
	// reports the actual cause (websocket close code, TCP reset, …) instead
	// of a bare "transport closed" — a live remote-agent failure left no
	// diagnostic in the run output.
	if readErr := sc.Err(); readErr != nil {
		s.failPending(fmt.Errorf("acp: transport closed: %v (agent exited without responding)", readErr))
	} else {
		s.failPending(fmt.Errorf("acp: transport closed (agent exited without responding)"))
	}
}

// failPending delivers a terminal error to all in-flight RPC calls. Called
// once by startReader when the read side closes.
func (s *Session) failPending(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, call := range s.pending {
		call.resp.Error = &Error{Code: ErrorCodeInternal, Message: err.Error()}
		close(call.done)
		delete(s.pending, id)
	}
}

// handleAgentRequest dispatches an incoming Agent→Client RPC request
// and writes the JSON-RPC 2.0 response back to the agent on stdin.
func (s *Session) handleAgentRequest(method string, id json.RawMessage, params json.RawMessage) {
	s.mu.Lock()
	h := s.clientReqH
	s.mu.Unlock()

	if h == nil {
		s.writeRPCResponse(id, nil, fmt.Errorf("agent→client RPC not configured"))
		return
	}

	var resp any
	var err error

	switch method {
	case "session/request_permission":
		var req RequestPermissionRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleRequestPermission(context.Background(), req)
		}
	case "fs/read_text_file":
		var req ReadTextFileRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleReadTextFile(context.Background(), req)
		}
	case "fs/write_text_file":
		var req WriteTextFileRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleWriteTextFile(context.Background(), req)
		}
	case "terminal/create":
		var req CreateTerminalRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleCreateTerminal(context.Background(), req)
		}
	case "terminal/output":
		var req TerminalOutputRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleTerminalOutput(context.Background(), req)
		}
	case "terminal/wait_for_exit":
		var req WaitForTerminalExitRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleWaitForTerminalExit(context.Background(), req)
		}
	case "terminal/kill":
		var req KillTerminalRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleKillTerminal(context.Background(), req)
		}
	case "terminal/release":
		var req ReleaseTerminalRequest
		if json.Unmarshal(params, &req) == nil {
			resp, err = h.HandleReleaseTerminal(context.Background(), req)
		}
	}

	if err != nil {
		s.writeRPCResponse(id, nil, err)
		return
	}
	s.writeRPCResponse(id, resp, nil)
}

// writeRPCResponse sends a JSON-RPC 2.0 response (or error) to the agent
// via stdin.
func (s *Session) writeRPCResponse(id json.RawMessage, result any, err error) {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *Error          `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id}

	if err != nil {
		resp.Error = &Error{Code: ErrorCodeInternal, Message: err.Error()}
	} else {
		resp.Result, _ = json.Marshal(result)
	}

	data, _ := json.Marshal(resp)
	s.writeMu.Lock()
	s.w.Write(append(data, '\n'))
	s.writeMu.Unlock()
}

// dispatchNotif routes a incoming notification to the EventHandler.
// Unrecognized notification methods are silently ignored per ACP spec.
func (s *Session) dispatchNotif(method string, params json.RawMessage) {
	switch method {
	case "session/update":
		s.dispatchSessionUpdate(params)
	case "$/cancel_request":
		s.handleCancelRequestNotif(params)
	}
}

// handleCancelRequestNotif handles an incoming $/cancel_request notification
// from the agent.  The agent is asking us to cancel an in-flight client→agent
// request.  We cancel the pending call so the blocked goroutine unblocks with
// a context-cancelled error.
func (s *Session) handleCancelRequestNotif(params json.RawMessage) {
	var notif CancelRequestNotification
	if json.Unmarshal(params, &notif) != nil {
		return
	}
	// Close the pending call AND remove it from the map under the same lock.
	// Without the delete, a subsequent transport-EOF failPending would find the
	// same entry and close(call.done) a second time → panic (close of closed
	// channel), crashing the reader goroutine (no recover).
	s.mu.Lock()
	call, ok := s.pending[notif.RequestID]
	if ok {
		delete(s.pending, notif.RequestID)
		if call != nil {
			// Deliver a synthetic cancellation error so the blocked caller
			// returns promptly.
			call.resp.Error = &Error{Code: ErrorCodeRequestCancelled, Message: "request cancelled by agent"}
			close(call.done)
		}
	}
	s.mu.Unlock()
}

func (s *Session) dispatchSessionUpdate(params json.RawMessage) {
	var notif SessionNotification
	if json.Unmarshal(params, &notif) != nil {
		return
	}
	s.mu.Lock()
	h := s.eh
	s.mu.Unlock()
	if h == nil {
		return
	}

	u := notif.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if cb := u.ContentAsBlock(); cb != nil {
			h.OnAgentMessage(cb.Text)
		}
	case "agent_thought_chunk":
		if cb := u.ContentAsBlock(); cb != nil {
			h.OnAgentThought(cb.Text)
		}
	case "user_message_chunk":
		if cb := u.ContentAsBlock(); cb != nil {
			h.OnUserMessage(cb.Text)
		}
	case "tool_call", "tool_call_update":
		title := ""
		if u.Title != nil {
			title = *u.Title
		}
		h.OnToolCall(ToolCallUpdate{
			ToolCallID: u.ToolCallID, Title: title,
			Kind: u.Kind, Status: u.Status,
			RawInput: u.RawInput, RawOutput: u.RawOutput,
			Content: u.ContentAsToolCallContent(), Locations: u.Locations,
		})
	case "plan":
		h.OnPlan(Plan{Entries: u.Entries})
	case "available_commands_update":
		h.OnAvailableCommandsUpdate(u.AvailableCommands)
	case "current_mode_update":
		h.OnModeUpdate(u.CurrentModeID)
	case "config_option_update":
		h.OnConfigOptionUpdate(u.ConfigOptions)
	case "usage_update":
		used, total := 0, 0
		if u.Used != nil {
			used = *u.Used
		}
		if u.Size != nil {
			total = *u.Size
		}
		h.OnUsageUpdate(used, total, u.Cost)
	case "session_info_update":
		title := ""
		if u.Title != nil {
			title = *u.Title
		}
		h.OnSessionInfo(title, u.Meta)
	}
}
