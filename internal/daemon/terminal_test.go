package daemon

import (
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/acp"
)

// TestTerminalLifecycle covers create → incremental poll → wait → release:
// the happy path every agent exercises.
func TestTerminalLifecycle(t *testing.T) {
	tm := newTerminalManager()
	id, err := tm.create("sh", []string{"-c", "echo one; sleep 0.2; echo two"}, nil, "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("create: empty terminal id")
	}
	if tm.activeCount() != 1 {
		t.Fatalf("activeCount: want 1, got %d", tm.activeCount())
	}

	// Polls are incremental; the process needs a moment to emit "one".
	var first *acp.TerminalOutputResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		first, err = tm.output(id)
		if err != nil {
			t.Fatalf("first poll: %v", err)
		}
		if strings.Contains(first.Output, "one") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if first.Truncated {
		t.Fatal("output: unexpectedly truncated")
	}
	if !strings.Contains(first.Output, "one") {
		t.Fatalf("first poll output %q missing %q", first.Output, "one")
	}
	if first.ExitStatus != nil {
		t.Fatal("first poll: command should still be running")
	}

	wait, err := tm.wait(id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if wait.ExitCode == nil || *wait.ExitCode != 0 {
		t.Fatalf("wait: want exit 0, got %+v", wait)
	}
	// After exit, a poll returns the tail (the last poll's output).
	tail, err := tm.output(id)
	if err != nil {
		t.Fatalf("tail poll: %v", err)
	}
	if !strings.Contains(tail.Output, "two") {
		t.Fatalf("tail poll output %q missing %q", tail.Output, "two")
	}
	if tail.ExitStatus == nil || *tail.ExitStatus.ExitCode != 0 {
		t.Fatalf("tail poll: want exit status 0, got %+v", tail.ExitStatus)
	}
	if tm.activeCount() != 0 {
		t.Fatalf("activeCount after exit: want 0, got %d", tm.activeCount())
	}

	if err := tm.release(id); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Released terminals are unknown.
	if _, err := tm.output(id); err == nil {
		t.Fatal("output after release: want unknown-terminal error")
	}
}

// TestTerminalKillSignals: kill mid-run makes wait return signaled and the
// terminal stop counting as active.
func TestTerminalKillSignals(t *testing.T) {
	tm := newTerminalManager()
	id, err := tm.create("sleep", []string{"100"}, nil, "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tm.kill(id); err != nil {
		t.Fatalf("kill: %v", err)
	}
	wait, err := tm.wait(id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if wait.Signal == nil || *wait.Signal == "" {
		t.Fatalf("wait after kill: want signal, got %+v", wait)
	}
	if tm.activeCount() != 0 {
		t.Fatalf("activeCount after kill: want 0, got %d", tm.activeCount())
	}
}

// TestTerminalKillProcessGroup: `sh -c "sleep 100 & sleep 100"` must die as a
// GROUP — a backgrounded child cannot escape cleanup as an orphan.
func TestTerminalKillProcessGroup(t *testing.T) {
	tm := newTerminalManager()
	id, err := tm.create("sh", []string{"-c", "sleep 100 & sleep 100"}, nil, "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tm.kill(id); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// The process group (pgid = sh's pid) must be gone shortly after.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pgid := tm.get(id).cmd.Process.Pid
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pgid := tm.get(id).cmd.Process.Pid
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group %d still alive after kill", pgid)
	}
}

// TestTerminalOutputByteLimit: an endless stream slides the buffer to the
// cap and marks the output truncated.
func TestTerminalOutputByteLimit(t *testing.T) {
	tm := newTerminalManager()
	// 1000 lines of "0123456789\n" — 11k bytes, cap at 100.
	id, err := tm.create("sh", []string{"-c", "seq 1 1000 | while read i; do echo 0123456789; done"}, nil, "", 100)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wait, err := tm.wait(id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if wait.ExitCode == nil || *wait.ExitCode != 0 {
		t.Fatalf("wait: want 0, got %+v", wait)
	}
	out, err := tm.output(id)
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if !out.Truncated {
		t.Fatal("output: want truncated=true for an 11k stream with a 100-byte cap")
	}
	if len(out.Output) > 100 {
		t.Fatalf("output: buffer exceeded cap (%d bytes)", len(out.Output))
	}
}

// TestTerminalCleanupKillsAll: cleanup terminates every leftover, including
// a command that would run forever.
func TestTerminalCleanupKillsAll(t *testing.T) {
	tm := newTerminalManager()
	if _, err := tm.create("sleep", []string{"100"}, nil, "", 0); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := tm.create("sleep", []string{"100"}, nil, "", 0); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	tm.cleanup()
	if tm.activeCount() != 0 {
		t.Fatalf("activeCount after cleanup: want 0, got %d", tm.activeCount())
	}
}

// TestTerminalWaitTwice: a second wait on the same terminal returns
// immediately (already exited).
func TestTerminalWaitTwice(t *testing.T) {
	tm := newTerminalManager()
	id, err := tm.create("echo", []string{"x"}, nil, "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := tm.wait(id); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := tm.wait(id); err != nil {
			t.Errorf("second wait: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second wait did not return immediately")
	}
}

// TestTerminalShellSemantics: per the ACP protocol, command is the final
// executable — an agent that wants shell semantics passes `sh -c` itself.
// Pipes and variable expansion work through that explicit shell, and the
// injected run env reaches it.
func TestTerminalShellSemantics(t *testing.T) {
	tm := newTerminalManager()
	// Agent passes sh -c with a pipeline.
	id, err := tm.create("sh", []string{"-c", "echo hello | tr a-z A-Z"}, nil, "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := tm.wait(id); err != nil {
		t.Fatalf("wait: %v", err)
	}
	out, err := tm.output(id)
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if !strings.Contains(out.Output, "HELLO") {
		t.Fatalf("pipeline output %q missing %q", out.Output, "HELLO")
	}

	// Variable expansion reaches the injected run env.
	id2, err := tm.create("sh", []string{"-c", "echo $AGENTWORK_RUN_ID"}, []string{"AGENTWORK_RUN_ID=run-x"}, "", 0)
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := tm.wait(id2); err != nil {
		t.Fatalf("wait 2: %v", err)
	}
	out2, err := tm.output(id2)
	if err != nil {
		t.Fatalf("output 2: %v", err)
	}
	if !strings.Contains(out2.Output, "run-x") {
		t.Fatalf("variable expansion output %q missing %q", out2.Output, "run-x")
	}
}

// TestTerminalEnvInjection: the run context lands in the command's
// environment, overriding anything the agent passes.
func TestTerminalEnvInjection(t *testing.T) {
	env := newRunEnvironment("run-1", "goal-1", "agent-1", t.TempDir(), "http://127.0.0.1:7373", "/opt/bin")
	got := env.runEnv([]acp.EnvVariable{{Name: "AGENTWORK_GOAL_ID", Value: "spoofed"}, {Name: "MY_VAR", Value: "kept"}})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"AGENTWORK_GOAL_ID=goal-1", // run context wins over the agent's spoof
		"AGENTWORK_RUN_ID=run-1",
		"AGENTWORK_AGENT_ID=agent-1",
		"AGENTWORK_SERVER_URL=http://127.0.0.1:7373",
		"MY_VAR=kept",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q in:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "PATH=/opt/bin:") {
		t.Fatalf("PATH not prepended with cli dir:\n%s", joined)
	}
}

// TestFSHandlers: read with line/limit slicing, write with parent dir
// creation, and error paths.
func TestFSHandlers(t *testing.T) {
	dir := t.TempDir()
	env := newRunEnvironment("run-1", "goal-1", "agent-1", dir, "http://127.0.0.1:7373", "")

	path := dir + "/nested/deep/file.txt"
	if _, err := env.HandleWriteTextFile(t.Context(), acp.WriteTextFileRequest{Path: path, Content: "a\nb\nc\nd\ne\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, limit := 2, 2
	read, err := env.HandleReadTextFile(t.Context(), acp.ReadTextFileRequest{Path: path, Line: &line, Limit: &limit})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Content != "b\nc" {
		t.Fatalf("read slice: want %q, got %q", "b\nc", read.Content)
	}

	if _, err := env.HandleReadTextFile(t.Context(), acp.ReadTextFileRequest{Path: dir + "/missing.txt"}); err == nil {
		t.Fatal("read missing file: want error")
	}
	// An unknown command fails at spawn (direct exec, per the protocol) —
	// create reports the error immediately.
	if _, err := env.HandleCreateTerminal(t.Context(), acp.CreateTerminalRequest{Command: "/nonexistent-binary"}); err == nil {
		t.Fatal("create with missing command: want error")
	}
}
