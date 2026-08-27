package daemon

// chatRelay is the daemon's half of the ACP chat relay (Phase 6): web ACP
// clients connect to GET /agents/{id}/chat; the daemon opens a machine-side
// chat channel and forwards ACP frames UNPARSED in both directions. The
// session lifecycle (new/list/load) is the protocol's own business — the
// daemon never reads the frames.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
)

// chatRelay keeps one entry per open web chat socket.
type chatRelay struct {
	mu    sync.Mutex
	chats map[string]*chatEntry // chatID → the channel
}

type chatEntry struct {
	machineID string
	cwd       string // the machine-resolved absolute chat directory
	// pump carries machine frames to the chat's ONE writer goroutine. The
	// /connect readLoop dispatches every incoming frame on its own
	// goroutine (peer.go: go dispatchRequest) — during an agent's reply
	// flood that is dozens of goroutines racing to the web socket, with no
	// order guarantee. The writer pump serializes them AND re-orders by the
	// machine-stamped seq (a scrambled agent reply is worse than a slow
	// one); done tears the writer down without ever closing pump (a send
	// on a closed channel would panic — senders select on done instead).
	pump      chan queuedFrame
	done      chan struct{}
	closeOnce sync.Once
	close     func() // → the web socket
}

// queuedFrame is one machine→web frame plus its ordering stamp.
type queuedFrame struct {
	seq  int64
	data []byte
}

// OpenChatForAgent validates the agent's machine and spawns the
// machine-side chat (the CLI process + ACP connection). Returns the chat id.
func (d *Daemon) OpenChatForAgent(ctx context.Context, agentID string) (string, error) {
	var machineID, argsJSON, envJSON, agentName string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.machine_id, r.args, r.env, a.name
		 FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`,
		agentID).Scan(&machineID, &argsJSON, &envJSON, &agentName); err != nil {
		return "", fmt.Errorf("unknown agent")
	}
	if machineID == "" {
		return "", fmt.Errorf("the agent's runtime has no machine")
	}
	peer := d.MachinePeer(machineID)
	if peer == nil {
		return "", fmt.Errorf("machine offline")
	}
	var args []string
	_ = json.Unmarshal([]byte(argsJSON), &args)
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(envJSON), &rtEnv)
	if rtEnv == nil {
		rtEnv = map[string]string{}
	}
	// MCP: reuse the run path's reader (extraMcpServers, daemon.go) so chat
	// and run share one source of truth for agent.mcp_servers. The machine
	// injects these into the ACP session/new frame (chat has no run token, so
	// unlike the run path they can't ride RunDispatchParams.McpServers).
	mcpServers := d.extraMcpServers(ctx, agentID)
	// No artificial deadline — the spawn takes as long as it takes (the
	// request context still bounds it by the connection's lifetime). A
	// timeout here only orphans the machine-side process: the CLI keeps
	// spawning while the daemon has already given up and can never learn
	// the chat id to clean it up.
	var res link.ChatOpenResult
	if err := peer.Call(ctx, link.MethodChatOpen, link.ChatOpenParams{
		AgentID:    agentID,
		ACPSpawn:   args,
		Env:        rtEnv,
		McpServers: mcpServers,
		ChatBrief:  buildChatBrief(agentName),
		// Cwd is omitted — the MACHINE resolves its own chat directory
		// (~/.agentwork/chat/<agentID>/); the path is machine-local.
	}, &res); err != nil {
		return "", fmt.Errorf("chat.open: %w", err)
	}
	d.chat.mu.Lock()
	if d.chat.chats == nil {
		d.chat.chats = map[string]*chatEntry{}
	}
	d.chat.chats[res.ChatID] = &chatEntry{
		machineID: machineID,
		cwd:       res.Cwd,
		pump:      make(chan queuedFrame, 256),
		done:      make(chan struct{}),
	}
	d.chat.mu.Unlock()
	logging.Infof("chat: %s opened for agent %s on machine %s", res.ChatID, agentID, machineID)
	return res.ChatID, nil
}

// BindChatSink attaches the web socket to an opened chat and starts the
// chat's writer pump: exactly one goroutine writes the web socket, draining
// the frame queue in seq order. A failed write means the socket is gone
// (or half-dead) — close it so the handler's read wakes up and tears the
// chat down, matching the link's close-on-write-failure contract.
func (d *Daemon) BindChatSink(chatID string, write func([]byte) error, closeFn func()) {
	d.chat.mu.Lock()
	e, ok := d.chat.chats[chatID]
	if !ok {
		d.chat.mu.Unlock()
		return
	}
	e.close = closeFn
	d.chat.mu.Unlock()
	go func() {
		// Re-order buffer: frames arrive in ANY order (the /connect read
		// loop spawns one goroutine per frame); the machine's seq restores
		// the stream. next is the seq expected; pending holds out-of-order
		// arrivals. A gap is almost always a SLOW dispatch goroutine, not
		// a lost frame (frames are only truly lost at teardown) — so a
		// short patience window lets the missing frame catch up and keeps
		// perfect order; only a persisting gap flushes through. The gap
		// timer merely signals the pump — the pump stays the single writer.
		var next int64 = 1
		ordered := true
		pending := map[int64][]byte{}
		const gapPatience = 100 * time.Millisecond
		gapCh := make(chan struct{}, 1)
		gapTimer := time.AfterFunc(gapPatience, func() {
			select {
			case gapCh <- struct{}{}:
			default:
			}
		})
		gapTimer.Stop()
		gapArmed := false
		emit := func(b []byte) bool {
			if err := write(b); err != nil {
				logging.Infof("chat: %s web write: %v — closing the chat socket", chatID, err)
				closeFn()
				return false
			}
			return true
		}
		flushThrough := func() bool {
			keys := make([]int64, 0, len(pending))
			for k := range pending {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			for _, k := range keys {
				if !emit(pending[k]) {
					return false
				}
				next = k + 1
				delete(pending, k)
			}
			return true
		}
		for {
			select {
			case f := <-e.pump:
				if f.seq == 0 {
					// Legacy CLI (no seq): the stream cannot be re-ordered
					// — flush whatever is held and pass frames through.
					if ordered && !flushThrough() {
						return
					}
					ordered = false
					if !emit(f.data) {
						return
					}
					continue
				}
				if !ordered {
					if !emit(f.data) {
						return
					}
					continue
				}
				if f.seq < next {
					continue // late duplicate — its gap already flushed
				}
				pending[f.seq] = f.data
				if len(pending) >= 256 {
					// Pathological: frames genuinely lost — flush through
					// so the stream resumes (order sacrificed only here).
					if !flushThrough() {
						return
					}
					if gapArmed {
						gapTimer.Stop()
						gapArmed = false
					}
					continue
				}
				for {
					b, ok := pending[next]
					if !ok {
						break
					}
					delete(pending, next)
					next++
					if !emit(b) {
						return
					}
				}
				if len(pending) > 0 {
					// A gap opened — give the missing frame a moment.
					if !gapArmed {
						gapTimer.Reset(gapPatience)
						gapArmed = true
					}
				} else if gapArmed {
					gapTimer.Stop()
					gapArmed = false
				}
			case <-gapCh:
				// The gap outlived its patience — flush through it.
				gapArmed = false
				if !flushThrough() {
					return
				}
			case <-e.done:
				gapTimer.Stop()
				return
			}
		}
	}()
}

// ChatWrite forwards one web frame to the machine's chat channel, with
// ONE piece of normalization: session/new and session/load get the
// machine-resolved absolute cwd (opencode REQUIRES cwd and resolves a
// '~'-shaped value against the process cwd into a doubled path — the web
// must never invent machine-local paths).
func (d *Daemon) ChatWrite(chatID string, frame []byte) error {
	d.chat.mu.Lock()
	e, ok := d.chat.chats[chatID]
	d.chat.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown chat %s", chatID)
	}
	peer := d.MachinePeer(e.machineID)
	if peer == nil {
		return fmt.Errorf("machine offline")
	}
	frame = normalizeChatFrame(frame, e.cwd)
	logChatFrame("web→machine", frame)
	// No artificial deadline: an agent turn takes as long as it takes, and
	// the frame write only fails when the machine link itself dies (the
	// peer's write-failure-closes-the-conn contract) — that is the error
	// the caller should see, not a clock.
	return peer.Call(context.Background(), link.MethodChatFrame, link.ChatFrameParams{
		ChatID: chatID,
		Frame:  append(json.RawMessage(nil), frame...),
	}, nil)
}

// normalizeChatFrame injects the chat directory into session/new, and
// replaces a missing or non-absolute cwd on session/load with it.
func normalizeChatFrame(frame []byte, chatCwd string) []byte {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Method == "" {
		return frame
	}
	var params map[string]any
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &params)
	}
	if params == nil {
		params = map[string]any{}
	}
	switch msg.Method {
	case "session/new":
		params["cwd"] = chatCwd
	case "session/load":
		cwd, _ := params["cwd"].(string)
		if !strings.HasPrefix(cwd, "/") {
			params["cwd"] = chatCwd
		}
	default:
		return frame
	}
	b, err := json.Marshal(params)
	if err != nil {
		return frame
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(frame, &m)
	m["params"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return frame
	}
	return out
}

// logChatFrame logs a chat frame's method. Permission traffic additionally
// logs the params/result — the relay's blindness hurts most exactly there:
// the options the agent offered and the outcome the web sent must be
// visible in OUR logs when an approval misbehaves.
func logChatFrame(dir string, frame []byte) {
	var probe struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		logging.Infof("chat: %s unparseable frame", dir)
		return
	}
	// Per-frame relay traffic is trace noise (one agent reply = dozens of
	// session/update lines) — debug level; the permission params/outcome
	// below are rare and decisive, so they stay at info.
	logging.Debugf("chat: %s %s", dir, probe.Method)
	if probe.Method == "session/request_permission" {
		logging.Infof("chat: %s permission options: %s", dir, probe.Params)
	}
	if probe.Method == "" && len(probe.ID) > 0 && bytes.Contains(probe.Result, []byte("outcome")) {
		logging.Infof("chat: %s permission outcome: %s", dir, probe.Result)
	}
}

// MachineChatFrame queues a chat.frame notification (machine → daemon) for
// the chat's writer pump. The frame bytes are COPIED: they slice into the
// peer's read buffer, which is reused once this dispatch returns.
func (d *Daemon) MachineChatFrame(p link.ChatFrameParams) {
	d.chat.mu.Lock()
	e := d.chat.chats[p.ChatID]
	d.chat.mu.Unlock()
	if e == nil {
		return
	}
	logChatFrame("machine→web", p.Frame)
	select {
	case e.pump <- queuedFrame{seq: p.Seq, data: append([]byte(nil), p.Frame...)}:
	case <-e.done:
		// teardown raced the dispatch — drop
	}
}

// MachineChatClosed closes the web socket when the CLI exited.
func (d *Daemon) MachineChatClosed(p link.ChatClosedParams) {
	d.chat.mu.Lock()
	e := d.chat.chats[p.ChatID]
	delete(d.chat.chats, p.ChatID)
	d.chat.mu.Unlock()
	logging.Infof("chat: %s closed (machine-side): %s", p.ChatID, p.Reason)
	if e == nil {
		return
	}
	e.closeOnce.Do(func() { close(e.done) })
	if e.close != nil {
		e.close()
	}
}

// CloseChat tears the machine chat down (the web socket disconnected).
func (d *Daemon) CloseChat(chatID string) {
	d.chat.mu.Lock()
	e := d.chat.chats[chatID]
	delete(d.chat.chats, chatID)
	d.chat.mu.Unlock()
	if e == nil {
		return
	}
	e.closeOnce.Do(func() { close(e.done) })
	if peer := d.MachinePeer(e.machineID); peer != nil {
		_ = peer.Notify(context.Background(), link.MethodChatClose, link.ChatCloseParams{ChatID: chatID})
	}
	logging.Infof("chat: %s closed (web-side)", chatID)
}
