// Terminal management for the client execution-environment proxy
// (DESIGN.md 决策 4-8): remote ACP servers execute commands through
// Agent→Client terminal RPCs, and the client (agentwork) runs those
// commands on the platform machine. This file owns the per-run terminal
// state.
//
// Lifecycle: one terminalId = one command instance (tool-level, per the
// ACP protocol — there is no interactive persistent shell). The agent
// creates, polls, waits, kills and releases during its turn; the platform
// unconditionally cleans up all leftovers at session close (cleanup). A
// cross-run terminal has no existence value — the platform guarantees the
// end state is clean, no list is surfaced to the agent, no decision is
// left to it.
//
// Output polling is INCREMENTAL: each output request returns only the
// bytes produced since the previous poll, so wait_for_exit followed by a
// final poll still sees the tail. OutputByteLimit slides the buffer
// (oldest dropped, Truncated=true) so an endless stream (tail -f) cannot
// exhaust memory.
//
// Commands are killed as a PROCESS GROUP (Setpgid, kill -pgid) so
// `sh -c "nohup x &"` cannot escape the session as an orphan.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"github.com/eushing/agentwork/internal/acp"
)

// outputBuffer is a thread-safe append-only byte buffer with an
// incremental-read cursor and an optional sliding size cap.
type outputBuffer struct {
	mu        sync.Mutex
	data      []byte
	consumed  int
	truncated bool
	limit     int // 0 = unlimited
}

func newOutputBuffer(limit int) *outputBuffer {
	return &outputBuffer{limit: limit}
}

// Write appends p, sliding the window if the cap is exceeded. Implements
// io.Writer so output streams can io.Copy into the buffer.
func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if b.limit > 0 {
		// Slide: drop the oldest bytes so the buffer stays ≤ limit. drop is
		// capped at len(b.data) — new content p is always kept in full. The
		// cut lands on a UTF-8 character boundary (the protocol requires
		// truncation to keep valid string output): a cut that would split a
		// multi-byte rune slides forward to the next boundary.
		total := len(b.data) + len(p)
		if total > b.limit {
			b.truncated = true
			drop := total - b.limit
			if drop > len(b.data) {
				drop = len(b.data)
			}
			for drop < len(b.data) && !utf8.RuneStart(b.data[drop]) {
				drop++
			}
			b.data = b.data[drop:]
			if b.consumed > drop {
				b.consumed -= drop
			} else {
				b.consumed = 0
			}
		}
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

// read returns the bytes written since the last read and advances the
// cursor. Callers should NOT hold any other buffer lock.
func (b *outputBuffer) read() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.data[b.consumed:]
	b.consumed = len(b.data)
	return out, b.truncated
}

// termState is one running (or finished) command instance.
type termState struct {
	id     string
	cmd    *exec.Cmd
	buf    *outputBuffer
	done   chan struct{} // closed when the process exits
	exitMu sync.Mutex
	exited bool
	code   int
	signal string
}

// terminalManager owns the run's terminals. Per-run instance: created at
// run start, cleaned up unconditionally at session close.
type terminalManager struct {
	mu    sync.Mutex
	terms map[string]*termState
	// nextID keeps terminal ids unique within the run.
	nextID atomic.Int64
}

func newTerminalManager() *terminalManager {
	return &terminalManager{terms: make(map[string]*termState)}
}

// activeCount reports how many terminals are still running (used by the
// idle watchdog: an agent polling a long command is silent on the event
// stream and must not be killed by the short idle window).
func (m *terminalManager) activeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.terms {
		t.exitMu.Lock()
		running := !t.exited
		t.exitMu.Unlock()
		if running {
			n++
		}
	}
	return n
}

// create spawns one command and returns its terminal id. env is the final
// environment (the caller has already layered platform + agent env);
// cwd defaults to the run's workdir when empty.
//
// Per the ACP protocol (terminal/create), command is the FINAL executable
// and args the argument array — the client spawns it directly, it does not
// interpret shell syntax. An agent that needs shell semantics passes
// `sh -c "..."` itself (the protocol example: command "npm", args
// ["test", "--coverage"]).
func (m *terminalManager) create(command string, args []string, env []string, cwd string, byteLimit int) (acp.TerminalId, error) {
	if command == "" {
		return "", errors.New("terminal: empty command")
	}
	cmd := exec.Command(command, args...)
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	// Process group so cleanup/kill takes the whole tree with it — a
	// `sh -c "nohup x &"` cannot outlive the session as an orphan.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	buf := newOutputBuffer(byteLimit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("terminal stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("terminal stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("terminal start: %w", err)
	}
	id := acp.TerminalId(fmt.Sprintf("t%d", m.nextID.Add(1)))

	t := &termState{id: string(id), cmd: cmd, buf: buf, done: make(chan struct{})}
	m.mu.Lock()
	m.terms[string(id)] = t
	m.mu.Unlock()

	// Both pipes drain into the same buffer (interleaved, like a real
	// terminal). The reaper goroutine records the exit and closes done.
	go func() {
		_, _ = io.Copy(buf, stdout)
		_, _ = io.Copy(buf, stderr)
		code, signal := 0, ""
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					signal = ws.Signal().String()
					code = -1
				}
			} else {
				log.Printf("daemon: terminal %s wait: %v", id, err)
				code = -1
			}
		}
		t.exitMu.Lock()
		t.exited = true
		t.code = code
		t.signal = signal
		t.exitMu.Unlock()
		close(t.done)
	}()
	return id, nil
}

// output returns the incremental output since the last poll plus the exit
// status if the command has finished.
func (m *terminalManager) output(id acp.TerminalId) (*acp.TerminalOutputResponse, error) {
	t := m.get(id)
	if t == nil {
		return nil, fmt.Errorf("terminal %q: unknown", id)
	}
	out, truncated := t.buf.read()
	resp := &acp.TerminalOutputResponse{Output: string(out), Truncated: truncated}
	t.exitMu.Lock()
	if t.exited {
		resp.ExitStatus = &acp.TerminalExitStatus{ExitCode: &t.code, Signal: &t.signal}
	}
	t.exitMu.Unlock()
	return resp, nil
}

// wait blocks until the command exits and returns its exit status.
func (m *terminalManager) wait(id acp.TerminalId) (*acp.WaitForTerminalExitResponse, error) {
	t := m.get(id)
	if t == nil {
		return nil, fmt.Errorf("terminal %q: unknown", id)
	}
	<-t.done
	t.exitMu.Lock()
	defer t.exitMu.Unlock()
	return &acp.WaitForTerminalExitResponse{ExitCode: &t.code, Signal: &t.signal}, nil
}

// kill terminates the command's process group.
func (m *terminalManager) kill(id acp.TerminalId) error {
	t := m.get(id)
	if t == nil {
		return fmt.Errorf("terminal %q: unknown", id)
	}
	return m.killState(t)
}

// release kills (if still running) and forgets the terminal. Per the
// protocol, releasing resources requires the command to be terminated.
func (m *terminalManager) release(id acp.TerminalId) error {
	m.mu.Lock()
	t, ok := m.terms[string(id)]
	if ok {
		delete(m.terms, string(id))
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("terminal %q: unknown", id)
	}
	return m.killState(t)
}

// cleanup kills every terminal still alive and forgets them all. Called
// unconditionally at session close — the platform guarantees the end
// state is clean.
func (m *terminalManager) cleanup() {
	m.mu.Lock()
	terms := m.terms
	m.terms = make(map[string]*termState)
	m.mu.Unlock()
	for _, t := range terms {
		_ = m.killState(t)
	}
}

// killState kills one terminal's process group if still running.
func (m *terminalManager) killState(t *termState) error {
	t.exitMu.Lock()
	running := !t.exited
	t.exitMu.Unlock()
	if !running {
		return nil
	}
	// Kill the whole process group (the command may have spawned children).
	pgid := t.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminal %s kill: %w", t.id, err)
	}
	return nil
}

func (m *terminalManager) get(id acp.TerminalId) *termState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terms[string(id)]
}
