// Client execution-environment proxy (DESIGN.md 决策 4-8): the run's
// agent (local stdio or remote ws/tcp) performs file reads/writes and
// command execution through Agent→Client RPCs handled here. The worktree
// always stays on the platform machine — the work a remote agent does is
// the work the daemon can verify and commit.
//
// Trust boundary: daemon user permissions, exactly like a stdio
// subprocess. No path whitelists/blacklists (same risk surface as stdio —
// sensitive-file filtering is a deferred item in DESIGN.md).
//
// Run context is injected at terminal/create (env + PATH): agentwork-cli
// is on the local PATH and its default SERVER_URL reaches this daemon, so
// collaboration works with zero CLI changes and no server-address
// knowledge in the agent.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eushing/agentwork/internal/acp"
)

// runEnvironment implements acp.ClientRequestHandler for one run. Created
// per run in runTask, registered on the ACP session, and dropped at
// session close — terminal leftovers are killed by tm.cleanup at the same
// moment.
type runEnvironment struct {
	runID, goalID, agentID string
	workdir                string // the goal's worktree (default terminal cwd)
	serverURL              string // AGENTWORK_SERVER_URL for the CLI
	cliPath                string // directory holding agentwork-cli (PATH)
	tm                     *terminalManager
}

// newRunEnvironment builds the per-run handler.
func newRunEnvironment(runID, goalID, agentID, workdir, serverURL, binDir string) *runEnvironment {
	return &runEnvironment{
		runID:     runID,
		goalID:    goalID,
		agentID:   agentID,
		workdir:   workdir,
		serverURL: serverURL,
		cliPath:   binDir,
		tm:        newTerminalManager(),
	}
}

// runEnv builds the environment for spawned commands: platform base +
// agent-requested env + run context injected last (authoritative).
func (e *runEnvironment) runEnv(agentEnv []acp.EnvVariable) []string {
	env := os.Environ()
	for _, kv := range agentEnv {
		env = append(env, kv.Name+"="+kv.Value)
	}
	// Run context — last wins over anything the agent passes.
	env = append(env,
		"AGENTWORK_GOAL_ID="+e.goalID,
		"AGENTWORK_RUN_ID="+e.runID,
		"AGENTWORK_AGENT_ID="+e.agentID,
		"AGENTWORK_SERVER_URL="+e.serverURL,
	)
	if e.cliPath != "" {
		env = append(env, "PATH="+e.cliPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return env
}

// ── fs ──

func (e *runEnvironment) HandleReadTextFile(ctx context.Context, req acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	if req.Line != nil || req.Limit != nil {
		content = sliceLines(content, deref(req.Line, 1), deref(req.Limit, 0))
	}
	return &acp.ReadTextFileResponse{Content: content}, nil
}

func (e *runEnvironment) HandleWriteTextFile(ctx context.Context, req acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
	if dir := filepath.Dir(req.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("write %s: %w", req.Path, err)
		}
	}
	if err := os.WriteFile(req.Path, []byte(req.Content), 0o644); err != nil {
		return nil, err
	}
	return &acp.WriteTextFileResponse{}, nil
}

// sliceLines returns lines [line, line+limit) (1-based). limit <= 0 means
// "to the end".
func sliceLines(content string, line, limit int) string {
	lines := strings.Split(content, "\n")
	if line < 1 {
		line = 1
	}
	if line > len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && line+limit-1 < end {
		end = line + limit - 1
	}
	return strings.Join(lines[line-1:end], "\n")
}

func deref(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// ── terminal ──

func (e *runEnvironment) HandleCreateTerminal(ctx context.Context, req acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
	cwd := e.workdir
	if req.Cwd != nil && *req.Cwd != "" {
		cwd = *req.Cwd
	}
	byteLimit := 0
	if req.OutputByteLimit != nil {
		byteLimit = *req.OutputByteLimit
	}
	id, err := e.tm.create(req.Command, req.Args, e.runEnv(req.Env), cwd, byteLimit)
	if err != nil {
		return nil, err
	}
	return &acp.CreateTerminalResponse{TerminalID: id}, nil
}

func (e *runEnvironment) HandleTerminalOutput(ctx context.Context, req acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
	return e.tm.output(req.TerminalID)
}

func (e *runEnvironment) HandleWaitForTerminalExit(ctx context.Context, req acp.WaitForTerminalExitRequest) (*acp.WaitForTerminalExitResponse, error) {
	return e.tm.wait(req.TerminalID)
}

func (e *runEnvironment) HandleKillTerminal(ctx context.Context, req acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
	if err := e.tm.kill(req.TerminalID); err != nil {
		return nil, err
	}
	return &acp.KillTerminalResponse{}, nil
}

func (e *runEnvironment) HandleReleaseTerminal(ctx context.Context, req acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
	if err := e.tm.release(req.TerminalID); err != nil {
		return nil, err
	}
	return &acp.ReleaseTerminalResponse{}, nil
}

// HandleRequestPermission is unsupported: approval is always human
// (DESIGN.md — 管家/agent 不能自动 approve).
func (e *runEnvironment) HandleRequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	return nil, fmt.Errorf("request_permission: not supported — approval is a human action")
}
