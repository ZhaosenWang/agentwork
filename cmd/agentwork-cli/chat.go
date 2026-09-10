package main

// chat.go — the machine's half of the ACP chat relay (Phase 6): chat.open
// spawns the agent's CLI with its ACP connection on stdio; ACP frames flow
// through /connect UNPARSED (chat.frame both directions); chat.close kills
// the process. The session lifecycle (new/list/load) is the CLI's own
// business — the bridge is transport only. The cwd is the agent's STABLE
// chat directory: the CLI keys its session store by cwd, so the web
// client's session/list + session/load work across connections.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/runtime"
)

type chatBridge struct {
	mu sync.Mutex
	// writer maps chatID → the CLI's stdin writer; closeFn kills the
	// process (the runtime's Close tears down the transport + child).
	writer  map[string]io.Writer
	closeFn map[string]func() error
	// writeMu maps chatID → a per-chat write mutex. stdin writes happen from
	// two goroutines: handleChatFrame (web→machine) and maybeAutoApprove
	// (stdout pump's permission auto-reply). Without a per-chat lock the two
	// writes interleave and corrupt the JSON-RPC stream.
	writeMu map[string]*sync.Mutex
	// mcpServers maps chatID → the agent's extra MCP servers (from
	// ChatOpenParams.McpServers, same source as the run path's). The
	// handleChatFrame rewriter injects them into the web client's session/new
	// frame — chat has no run dispatch, so this is the injection point the run
	// path doesn't need.
	mcpServers map[string][]acp.McpServer
	// seq is a MONOTONIC chat id counter — ids are never reused. A reused
	// id let a late chat.close (the ghost StrictMode connection's teardown,
	// delivered asynchronously) kill the REPLACEMENT process registered
	// under the same id (live: the active chat died with "file already
	// closed" seconds after opening).
	seq int
}

func newChatBridge() *chatBridge {
	return &chatBridge{
		writer:     map[string]io.Writer{},
		closeFn:    map[string]func() error{},
		writeMu:    map[string]*sync.Mutex{},
		mcpServers: map[string][]acp.McpServer{},
	}
}

// shutdown kills all live chat processes — called when the machine link
// drops (runLink's defer). Without this, agent CLI processes orphan: the
// link is dead so daemon cannot deliver chat.close, and the stdout pumps
// spin on Notify failures until the CLI exits on its own (which may be
// never — the agent waits for session/prompt). Each cleanup closes stdin
// (graceful) then force-kills after gracefulExitWait.
func (b *chatBridge) shutdown() {
	b.mu.Lock()
	ids := make([]string, 0, len(b.closeFn))
	for id := range b.closeFn {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.cleanup(id)
	}
}

// maybeAutoApprove checks if a stdout frame is a session/request_permission
// JSON-RPC request. If so, it auto-approves the strongest allow option
// (allow_always > allow_once) by writing the outcome response to the CLI's
// stdin, and returns true (the frame is NOT relayed to the web — the web
// never sees a permission popup). Returns false for all other frames (normal
// relay). Same trust boundary as the run path's autopermit.
func (b *chatBridge) maybeAutoApprove(line []byte, chatID string) bool {
	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &probe); err != nil || probe.Method != "session/request_permission" {
		return false
	}
	if len(probe.ID) == 0 {
		return false
	}
	var params struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(probe.Params, &params)
	// Pick allow_once > allow_always (narrowest first).
	var chosen string
	for _, o := range params.Options {
		if o.Kind == "allow_once" {
			chosen = o.OptionID
			break
		}
		if o.Kind == "allow_always" && chosen == "" {
			chosen = o.OptionID
		}
	}
	if chosen == "" {
		// No allow option — cancel (same as autopermit's fallback).
		chosen = ""
	}
	outcome := map[string]any{"outcome": "selected", "optionId": chosen}
	if chosen == "" {
		outcome = map[string]any{"outcome": "cancelled"}
	}
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      probe.ID,
		"result":  map[string]any{"outcome": outcome},
	})
	b.mu.Lock()
	w := b.writer[chatID]
	wmu := b.writeMu[chatID]
	b.mu.Unlock()
	if w != nil && wmu != nil {
		wmu.Lock()
		_, _ = w.Write(append(resp, '\n'))
		wmu.Unlock()
	}
	return true
}

// handleChatOpen spawns the agent's CLI, stages persona + skills into the
// chat cwd, and starts the stdout→/connect frame pump.
func (b *chatBridge) handleChatOpen(ctx context.Context, raw json.RawMessage, peer *link.Peer) (any, *link.RPCError) {
	var p link.ChatOpenParams
	if err := json.Unmarshal(raw, &p); err != nil || p.AgentID == "" || len(p.ACPSpawn) == 0 {
		return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "agent_id and acp_spawn are required"}
	}
	// The chat directory is MACHINE-local and STABLE per agent — the CLI
	// keys its session store by cwd, so session/list + session/load from
	// the web work across connections and browser refreshes.
	if p.Cwd == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, &link.RPCError{Code: link.CodeInternal, Message: "home: " + err.Error()}
		}
		p.Cwd = filepath.Join(home, ".agentwork", "chat", p.AgentID)
	}
	if err := os.MkdirAll(p.Cwd, 0o755); err != nil {
		return nil, &link.RPCError{Code: link.CodeInternal, Message: "mkdir chat cwd: " + err.Error()}
	}
	// Persona + chat brief + project skills — FRESH staging on every open:
	// the user may have edited the agent's system prompt / skills since the
	// last chat. The chat cwd is platform-owned (no repo content to preserve),
	// so overwrite the persona+brief and wipe the skills subdir before copying.
	// The brief rides AGENTS.md (the runtime loads it natively) — the chat path
	// has no prompt builder, so this is where the agent learns it is in a chat
	// surface and that `agentwork agent history` exists.
	persona, _ := os.ReadFile(filepath.Join(agentProfileDir(p.AgentID), "AGENTS.md"))
	agentsMd := strings.TrimSpace(string(persona))
	if p.ChatBrief != "" {
		if agentsMd != "" {
			agentsMd += "\n\n" + p.ChatBrief
		} else {
			agentsMd = p.ChatBrief
		}
	}
	if agentsMd != "" {
		_ = os.WriteFile(filepath.Join(p.Cwd, "AGENTS.md"), []byte(agentsMd), 0o644)
	}
	subdir := p.SkillsDir
	if subdir == "" {
		subdir = skillSubdir(p.ACPSpawn[0])
	}
	copyAgentSkills(p.AgentID, p.Cwd, subdir, true)

	// The spawn env = the machine's own env (base) + the agent's runtime
	// env (Spec.Env, layered by the runtime) + the agentwork CLI on PATH
	// (the agent may call it from chat). AGENTWORK_AGENT_ID lets the agent
	// run `agentwork agent history` from chat (no run token in the chat
	// path — the history endpoint resolves identity from this env, like the
	// run path's executor sets it at exec.go spawn).
	base := os.Environ()
	base = append(base, "AGENTWORK_AGENT_ID="+p.AgentID)
	base = append(base, "AGENTWORK_SERVER_URL="+os.Getenv("AGENTWORK_SERVER_URL"))
	if shimDir, err := ensureAgentworkShim(); err == nil && shimDir != "" {
		base = append(base, "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	conn, err := runtime.Open(ctx, runtime.Spec{
		Transport:  "stdio",
		Executable: p.ACPSpawn[0],
		Args:       p.ACPSpawn[1:],
		Env:        p.Env,
		Cwd:        p.Cwd,
	}, base)
	if err != nil {
		return nil, &link.RPCError{Code: link.CodeInternal, Message: "runtime: " + err.Error()}
	}
	chatID := fmt.Sprintf("chat-%d", b.nextID())
	b.mu.Lock()
	b.writer[chatID] = conn.W
	b.closeFn[chatID] = conn.Close
	b.writeMu[chatID] = &sync.Mutex{}
	if len(p.McpServers) > 0 {
		b.mcpServers[chatID] = p.McpServers
	}
	b.mu.Unlock()

	// stdout pump: one JSON-RPC frame per line → chat.frame notification,
	// stamped with a per-chat monotonic seq — the daemon's relay dispatch
	// races concurrent frames, and the seq lets its writer pump re-order
	// them (a reply flood must not read scrambled).
	//
	// session/request_permission frames are intercepted and auto-approved
	// (allow_always > allow_once) — same trust boundary as the run path's
	// autopermit. Chat is conversational, not a permission gate: IM (Feishu/
	// WeChat) has no UI to approve, and the popup on every bash call is noise.
	// The tool_call itself still streams via session/update (a separate frame).
	go func() {
		scanner := bufio.NewScanner(conn.R)
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		seq := int64(0)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			if b.maybeAutoApprove(line, chatID) {
				continue
			}
			seq++
			_ = peer.Notify(ctx, link.MethodChatFrame, link.ChatFrameParams{
				ChatID: chatID,
				Seq:    seq,
				Frame:  append(json.RawMessage(nil), line...),
			})
		}
		_ = peer.Notify(ctx, link.MethodChatClosed, link.ChatClosedParams{
			ChatID: chatID,
			Reason: fmt.Sprintf("cli exited: %v", scanner.Err()),
		})
		b.cleanup(chatID)
	}()
	cliLogf("chat: %s opened for agent %s (cwd=%s)", chatID, p.AgentID, p.Cwd)
	return link.ChatOpenResult{ChatID: chatID, Cwd: p.Cwd}, nil
}

// handleChatFrame writes one ACP frame to the CLI's stdin. For session/new it
// first injects the agent's extra MCP servers into the frame — the chat path
// has no run dispatch (the run path injects them via RunDispatchParams.McpServers
// at ACP session/new), so this rewriter is the chat path's injection point.
// Symmetric with the daemon's normalizeChatFrame (which injects cwd into
// session/new on the daemon→machine leg); this runs on the web→machine leg.
// Other methods pass through untouched (the relay stays blind outside this one
// configured injection, matching the cwd injection's discipline).
func (b *chatBridge) handleChatFrame(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
	var p link.ChatFrameParams
	if err := json.Unmarshal(raw, &p); err != nil || p.ChatID == "" {
		return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "chat_id is required"}
	}
	b.mu.Lock()
	w := b.writer[p.ChatID]
	mcp := b.mcpServers[p.ChatID]
	wmu := b.writeMu[p.ChatID]
	b.mu.Unlock()
	if w == nil {
		return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "unknown chat " + p.ChatID}
	}
	frame := p.Frame
	if len(mcp) > 0 {
		if rewritten := injectChatMcpServers(frame, mcp); rewritten != nil {
			frame = rewritten
			cliLogf("chat: %s session/new injected %d mcp server(s)", p.ChatID, len(mcp))
		}
	}
	wmu.Lock()
	_, err := w.Write(append(frame, '\n'))
	wmu.Unlock()
	if err != nil {
		return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
	}
	return map[string]bool{"ok": true}, nil
}

// injectChatMcpServers rewrites a session/new frame's params.mcpServers,
// replacing the web client's empty array with the agent's configured servers.
// It is the machine-side mirror of the daemon's normalizeChatFrame (which
// injects cwd): parse method/params → set one field → re-marshal the whole
// frame preserving id/jsonrpc/other params. Returns nil if the frame is not
// session/new or could not be parsed (the caller then writes the original —
// the relay degrades to blind pass-through, never to a dropped frame).
func injectChatMcpServers(frame []byte, mcpServers []acp.McpServer) []byte {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Method != "session/new" {
		return nil
	}
	var params map[string]json.RawMessage
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil
		}
	}
	if params == nil {
		params = map[string]json.RawMessage{}
	}
	// Normalize through the SAME path the run-path SDK uses (acp.NormalizeForWire
	// → normalizeNilSlices): strict ACP servers (opencode's zod) reject MISSING
	// arrays ("expected array, received undefined") — a stdio server's nil
	// headers/env must serialize as [], not vanish. Direct json.Marshal would
	// omit them (omitempty); the run path avoids this because the SDK applies
	// normalizeNilSlices to every request. The chat relay rewrites the frame
	// outside the SDK, so we apply it here to match.
	serversJSON, err := json.Marshal(acp.NormalizeForWire(mcpServers))
	if err != nil {
		return nil
	}
	params["mcpServers"] = serversJSON
	b, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(frame, &m); err != nil {
		return nil
	}
	m["params"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// handleChatClose kills the chat process.
func (b *chatBridge) handleChatClose(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
	var p link.ChatCloseParams
	_ = json.Unmarshal(raw, &p)
	b.cleanup(p.ChatID)
	return nil, nil
}

// gracefulExitWait is how long a chat process gets to exit on its own
// after its stdin closes before the force-kill lands.
const gracefulExitWait = 3 * time.Second

// cleanup removes the chat entry and tears the process down GRACEFULLY:
// stdin is closed first — a well-behaved ACP server exits on EOF and
// persists its session cleanly (an abrupt kill mid-turn is what left
// poisoned "empty assistant" history behind). The force-kill follows
// after a grace period for processes that ignore the EOF.
func (b *chatBridge) cleanup(chatID string) {
	b.mu.Lock()
	closeFn := b.closeFn[chatID]
	writer := b.writer[chatID]
	delete(b.writer, chatID)
	delete(b.closeFn, chatID)
	delete(b.mcpServers, chatID)
	delete(b.writeMu, chatID)
	b.mu.Unlock()
	if closeFn == nil {
		return
	}
	if wc, ok := writer.(io.Closer); ok {
		_ = wc.Close()
	}
	go func() {
		time.Sleep(gracefulExitWait)
		_ = closeFn()
	}()
}

// nextID returns an incrementing suffix for chat ids (never reused).
func (b *chatBridge) nextID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return b.seq
}
