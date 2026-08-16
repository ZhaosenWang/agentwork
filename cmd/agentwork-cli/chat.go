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
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/runtime"
)

type chatBridge struct {
	mu sync.Mutex
	// writer maps chatID → the CLI's stdin writer; closeFn kills the
	// process (the runtime's Close tears down the transport + child).
	writer  map[string]io.Writer
	closeFn map[string]func() error
	// seq is a MONOTONIC chat id counter — ids are never reused. A reused
	// id let a late chat.close (the ghost StrictMode connection's teardown,
	// delivered asynchronously) kill the REPLACEMENT process registered
	// under the same id (live: the active chat died with "file already
	// closed" seconds after opening).
	seq int
}

func newChatBridge() *chatBridge {
	return &chatBridge{writer: map[string]io.Writer{}, closeFn: map[string]func() error{}}
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
	// Persona + project skills — FRESH staging on every open: the user
	// may have edited the agent's system prompt / skills since the last
	// chat. The chat cwd is platform-owned (no repo content to preserve),
	// so overwrite the persona and wipe the skills subdir before copying.
	if b, err := os.ReadFile(filepath.Join(agentProfileDir(p.AgentID), "AGENTS.md")); err == nil {
		_ = os.WriteFile(filepath.Join(p.Cwd, "AGENTS.md"), b, 0o644)
	}
	subdir := p.SkillsDir
	if subdir == "" {
		subdir = skillSubdir(p.ACPSpawn[0])
	}
	copyAgentSkills(p.AgentID, p.Cwd, subdir, true)

	// The spawn env = the machine's own env (base) + the agent's runtime
	// env (Spec.Env, layered by the runtime) + the agentwork CLI on PATH
	// (the agent may call it from chat).
	base := os.Environ()
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
	b.mu.Unlock()

	// stdout pump: one JSON-RPC frame per line → chat.frame notification,
	// stamped with a per-chat monotonic seq — the daemon's relay dispatch
	// races concurrent frames, and the seq lets its writer pump re-order
	// them (a reply flood must not read scrambled).
	go func() {
		scanner := bufio.NewScanner(conn.R)
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		seq := int64(0)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
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

// handleChatFrame writes one ACP frame to the CLI's stdin.
func (b *chatBridge) handleChatFrame(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
	var p link.ChatFrameParams
	if err := json.Unmarshal(raw, &p); err != nil || p.ChatID == "" {
		return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "chat_id is required"}
	}
	b.mu.Lock()
	w := b.writer[p.ChatID]
	b.mu.Unlock()
	if w == nil {
		return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "unknown chat " + p.ChatID}
	}
	if _, err := w.Write(append(p.Frame, '\n')); err != nil {
		return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
	}
	return map[string]bool{"ok": true}, nil
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
