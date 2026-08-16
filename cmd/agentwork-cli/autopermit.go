package main

// autopermit is the executor's ClientRequestHandler: the machine runs
// autonomously — nobody sits at the terminal to click "allow". The
// platform's verdicts (verification + gates + human checkpoints) judge
// the run's OUTCOME, not its individual tool calls, so every permission
// request is answered with the strongest allow option the CLI offers, and
// the delegated fs/terminal RPCs are implemented natively (CLIs that lack
// their own file/terminal tools delegate them to the client).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/eushing/agentwork/internal/acp"
)

type autoTerm struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	out  bytes.Buffer
	done chan struct{}
}

type autopermit struct {
	mu    sync.Mutex
	terms map[string]*autoTerm
	cwd   string
}

func newAutopermit(cwd string) *autopermit {
	return &autopermit{terms: map[string]*autoTerm{}, cwd: cwd}
}

// HandleRequestPermission approves with the strongest allow option the
// CLI offered (allow_always > allow_once); only-reject menus cancel the
// request so the tool call fails visibly instead of hanging.
func (a *autopermit) HandleRequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	var opts []string
	for _, o := range req.Options {
		opts = append(opts, string(o.Kind)+":"+o.OptionID)
	}
	cliLogf("autopermit: permission request tool=%q title=%q kind=%s options=%v", req.ToolCall.Title, req.ToolCall.Kind, req.ToolCall.Status, opts)
	var allowOnce string
	for _, o := range req.Options {
		switch o.Kind {
		case acp.PermissionAllowAlways:
			id := o.OptionID
			cliLogf("autopermit: approved allow_always (%s)", id)
			return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{OptionID: &id}}, nil
		case acp.PermissionAllowOnce:
			allowOnce = o.OptionID
		}
	}
	if allowOnce != "" {
		cliLogf("autopermit: approved allow_once (%s)", allowOnce)
		return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{OptionID: &allowOnce}}, nil
	}
	cliLogf("autopermit: no allow option offered — cancelling")
	return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: true}}, nil
}

func (a *autopermit) HandleReadTextFile(ctx context.Context, req acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
	cliLogf("autopermit: fs read %s", req.Path)
	b, err := os.ReadFile(req.Path)
	if err != nil {
		cliLogf("autopermit: fs read %s failed: %v", req.Path, err)
		return nil, err
	}
	return &acp.ReadTextFileResponse{Content: string(b)}, nil
}

func (a *autopermit) HandleWriteTextFile(ctx context.Context, req acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
	cliLogf("autopermit: fs write %s (%d bytes)", req.Path, len(req.Content))
	if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(req.Path, []byte(req.Content), 0o644); err != nil {
		cliLogf("autopermit: fs write %s failed: %v", req.Path, err)
		return nil, err
	}
	return &acp.WriteTextFileResponse{}, nil
}

// Terminal delegation: a minimal native shell per terminal id — spawn,
// poll (the buffer resets after each read; the CLI polls incrementally),
// wait, kill, release.

func (a *autopermit) HandleCreateTerminal(ctx context.Context, req acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = a.cwd
	env := os.Environ()
	for _, e := range req.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	cmd.Env = env
	t := &autoTerm{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &t.out
	cmd.Stderr = &t.out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait(); close(t.done) }()
	a.mu.Lock()
	tid := fmt.Sprintf("term-%d", len(a.terms)+1)
	a.terms[tid] = t
	a.mu.Unlock()
	return &acp.CreateTerminalResponse{TerminalID: tid}, nil
}

func (a *autopermit) term(tid string) *autoTerm {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.terms[tid]
}

func (a *autopermit) HandleTerminalOutput(ctx context.Context, req acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
	t := a.term(req.TerminalID)
	if t == nil {
		return nil, fmt.Errorf("terminal %s not found", req.TerminalID)
	}
	t.mu.Lock()
	out := t.out.String()
	t.out.Reset()
	exited := false
	select {
	case <-t.done:
		exited = true
	default:
	}
	t.mu.Unlock()
	resp := &acp.TerminalOutputResponse{Output: out, Truncated: false}
	if exited {
		code := 0
		resp.ExitStatus = &acp.TerminalExitStatus{ExitCode: &code}
	}
	return resp, nil
}

func (a *autopermit) HandleWaitForTerminalExit(ctx context.Context, req acp.WaitForTerminalExitRequest) (*acp.WaitForTerminalExitResponse, error) {
	t := a.term(req.TerminalID)
	if t == nil {
		return nil, fmt.Errorf("terminal %s not found", req.TerminalID)
	}
	<-t.done
	return &acp.WaitForTerminalExitResponse{}, nil
}

func (a *autopermit) HandleKillTerminal(ctx context.Context, req acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
	t := a.term(req.TerminalID)
	if t == nil {
		return nil, fmt.Errorf("terminal %s not found", req.TerminalID)
	}
	_ = t.cmd.Process.Kill()
	return &acp.KillTerminalResponse{}, nil
}

func (a *autopermit) HandleReleaseTerminal(ctx context.Context, req acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
	t := a.term(req.TerminalID)
	if t != nil {
		_ = t.cmd.Process.Kill()
	}
	a.mu.Lock()
	delete(a.terms, req.TerminalID)
	a.mu.Unlock()
	return &acp.ReleaseTerminalResponse{}, nil
}
