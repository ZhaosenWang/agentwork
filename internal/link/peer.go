package link

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Handler responds to one method. Returning a non-nil *RPCError sends that
// error; otherwise result is marshalled into the response.
type Handler func(ctx context.Context, params json.RawMessage) (any, *RPCError)

// Peer is one end of the link over a WebSocket connection. Both sides use
// it: requests carry auto-incrementing string ids, responses are routed
// back to the waiting Call; notifications (no id) are dispatched without
// a reply. One JSON-RPC message per text frame.
//
// The reader loop starts inside NewPeer — a Peer left unserved must not
// deadlock its own Call (live bug: register wrote the request, nobody read
// the response, Ctrl+C could not even interrupt it). Wait blocks until the
// connection dies.
type Peer struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	mu       sync.Mutex
	nextID   atomic.Int64
	pending  map[string]chan *wireResponse
	handlers map[string]Handler
	done     chan struct{}
	exitErr  error
	closeOne sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
}

// wireRequest / wireResponse are the raw message shapes on the frame.
type wireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *string         `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type wireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *string         `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// writeTimeout bounds one frame write — a stalled peer must not hold the
// write lock (and a Caller) forever.
const writeTimeout = 10 * time.Second

// NewPeer wraps a live WebSocket connection and starts its reader loop.
func NewPeer(conn *websocket.Conn) *Peer {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Peer{
		conn:     conn,
		pending:  make(map[string]chan *wireResponse),
		handlers: make(map[string]Handler),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	go p.readLoop()
	return p
}

// Handle registers the handler for a method (overwrites).
func (p *Peer) Handle(method string, h Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handlers[method] = h
}

// Call sends a JSON-RPC request and blocks until the response arrives or
// ctx is cancelled / the link closes.
func (p *Peer) Call(ctx context.Context, method string, params, result any) error {
	resp, err := p.request(ctx, method, params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

func (p *Peer) request(ctx context.Context, method string, params any) (*wireResponse, error) {
	id := fmt.Sprintf("%d", p.nextID.Add(1))
	req := wireRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: rpcMarshal(params)}
	ch := make(chan *wireResponse, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	if err := p.write(&req); err != nil {
		return nil, fmt.Errorf("link: write %s: %w", method, err)
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, fmt.Errorf("link: closed while waiting for %s", method)
	}
}

// Notify sends a JSON-RPC notification (no id, no response).
func (p *Peer) Notify(ctx context.Context, method string, params any) error {
	msg := wireRequest{JSONRPC: "2.0", Method: method, Params: rpcMarshal(params)}
	if err := p.write(&msg); err != nil {
		return fmt.Errorf("link: notify %s: %w", method, err)
	}
	return nil
}

func (p *Peer) write(msg any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := p.conn.WriteJSON(msg); err != nil {
		// A failed (or deadline-expired) frame CORRUPTS the WebSocket
		// stream — gorilla's contract: the connection must die. A
		// half-dead link that still carries the other direction is the
		// worst failure mode (live: daemon→machine responses died while
		// heartbeats kept flowing and nothing ever detected it).
		p.Close()
		return err
	}
	return nil
}

// Wait blocks until the link dies or Close is called, returning the exit
// error (nil on clean close). The reader loop runs from NewPeer.
func (p *Peer) Wait() error {
	<-p.done
	return p.exitErr
}

// readLoop pumps frames until the connection dies. Incoming requests are
// dispatched to handlers; responses are routed to their pending Call.
func (p *Peer) readLoop() {
	err := p.readLoopErr()
	p.closeOne.Do(func() {
		p.exitErr = err
		p.cancel()
		close(p.done)
		_ = p.conn.Close()
	})
	// Wake every pending Call — they select on done.
}

func (p *Peer) readLoopErr() error {
	for {
		var raw json.RawMessage
		if err := p.conn.ReadJSON(&raw); err != nil {
			return err
		}
		// Discriminate request/notification (has "method") from response.
		var probe struct {
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   *RPCError       `json:"error"`
			ID      *string         `json:"id"`
			JSONRPC string          `json:"jsonrpc"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			p.replyParseError(raw)
			continue
		}
		if probe.Method == "" {
			if probe.ID == nil {
				continue // stray frame without id — nothing to route
			}
			resp := wireResponse{JSONRPC: "2.0", ID: probe.ID, Result: probe.Result, Error: probe.Error}
			p.routeResponse(&resp)
			continue
		}
		go p.dispatchRequest(probe.Method, probe.ID, raw)
	}
}

// replyParseError answers an unparseable frame with the standard error
// (best-effort — the peer may be gone).
func (p *Peer) replyParseError(_ json.RawMessage) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.WriteJSON(wireResponse{
		JSONRPC: "2.0", ID: nil,
		Error: &RPCError{Code: CodeParseError, Message: "parse error"},
	})
}

// routeResponse delivers a response frame to the waiting Call.
func (p *Peer) routeResponse(resp *wireResponse) {
	p.mu.Lock()
	ch := p.pending[*resp.ID]
	p.mu.Unlock()
	if ch == nil {
		return // no waiter (late response after ctx cancel) — drop
	}
	ch <- resp
}

// dispatchRequest runs a handler for an incoming request/notification and
// writes the response when the message carried an id. Handler panics are
// contained and answered as internal errors — a link handler must never
// take the whole process down.
func (p *Peer) dispatchRequest(method string, id *string, raw json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			if id != nil {
				p.writeMu.Lock()
				_ = p.conn.WriteJSON(wireResponse{JSONRPC: "2.0", ID: id,
					Error: &RPCError{Code: CodeInternal, Message: fmt.Sprintf("handler panic: %v", r)}})
				p.writeMu.Unlock()
			}
		}
	}()
	var params json.RawMessage
	var msg wireRequest
	if err := json.Unmarshal(raw, &msg); err == nil {
		params = msg.Params
	}
	p.mu.Lock()
	h := p.handlers[method]
	p.mu.Unlock()
	var result any
	var rpcErr *RPCError
	if h == nil {
		rpcErr = &RPCError{Code: CodeMethodNotFnd, Message: "method not found", Data: method}
	} else {
		result, rpcErr = h(p.ctx, params)
	}
	if id == nil {
		return // notification — no reply
	}
	// Route the response through write() — the deadline + the
	// close-on-failure contract apply to responses too (a swallowed
	// response-write error left the link half-dead and invisible).
	_ = p.write(wireResponse{JSONRPC: "2.0", ID: id, Result: rpcMarshal(result), Error: rpcErr})
}

// Close tears the connection down; Wait returns afterwards.
func (p *Peer) Close() {
	p.closeOne.Do(func() {
		p.exitErr = fmt.Errorf("link closed locally")
		p.cancel()
		close(p.done)
		_ = p.conn.Close()
	})
}

// Done is closed when the link is torn down.
func (p *Peer) Done() <-chan struct{} { return p.done }
