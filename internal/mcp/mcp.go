// Package mcp exposes the run's workspace as an MCP server over streamable
// HTTP, built on the official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// This is the ACP-v2-endorsed way for a client (agentwork) to hand its
// workspace to an agent that does not delegate its tools to client
// fs/terminal RPCs (opencode's tools are local by design) — MCP servers are
// a standard fixture of every mainstream agent, so the workspace tools
// appear in the agent's tool registry automatically. The server URL is
// advertised at ACP session/new (McpServers).
//
// The server is STATELESS (StreamableHTTPOptions.Stateless): each request
// maps to an Executor bound to one run's worktree + environment. See
// DESIGN.md 决策 4-8.
package mcp

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// errEmptyCommand guards the run_command tool against a missing executable.
var errEmptyCommand = errors.New("run_command: command is required")

// Executor binds the workspace tools to ONE run: the worktree as the
// filesystem basis and the run environment for commands. The daemon builds
// one per run and registers it under the run id.
type Executor struct {
	// Workdir is the run's worktree root — the workspace.
	Workdir string
	// Env is the command environment (platform base + run context, PATH
	// prepended with the agentwork-cli dir).
	Env []string
}

// NewExecutor binds an Executor to one run's workspace.
func NewExecutor(workdir string, env []string) *Executor {
	return &Executor{Workdir: workdir, Env: env}
}

// NewServer builds the MCP server with the three workspace tools registered.
func NewServer(exec *Executor) *gmcp.Server {
	srv := gmcp.NewServer(&gmcp.Implementation{Name: "agentwork", Version: "0.1"}, nil)

	type readArgs struct {
		Path string `json:"path" jsonschema:"absolute path of the file to read"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "read_file",
		Description: "Read a file from the workspace (absolute path).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args readArgs) (*gmcp.CallToolResult, any, error) {
		data, err := os.ReadFile(args.Path)
		if err != nil {
			return nil, nil, err
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(data)}}}, nil, nil
	})

	type writeArgs struct {
		Path    string `json:"path" jsonschema:"absolute path of the file to write"`
		Content string `json:"content" jsonschema:"file content"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "write_file",
		Description: "Write a file in the workspace (absolute path; parent directories are created).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args writeArgs) (*gmcp.CallToolResult, any, error) {
		if dir := dirOf(args.Path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, err
			}
		}
		if err := os.WriteFile(args.Path, []byte(args.Content), 0o644); err != nil {
			return nil, nil, err
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: "written " + args.Path}}}, nil, nil
	})

	type runArgs struct {
		Command string   `json:"command" jsonschema:"executable to run"`
		Args    *[]string `json:"args,omitempty" jsonschema:"command arguments"`
		Cwd     *string   `json:"cwd,omitempty" jsonschema:"working directory override (defaults to the workspace root)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "run_command",
		Description: "Run a command on the platform machine with the workspace as the working directory. Use for builds, tests, git and verification.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args runArgs) (*gmcp.CallToolResult, any, error) {
		var argv []string
		if args.Args != nil {
			argv = *args.Args
		}
		var cwd string
		if args.Cwd != nil {
			cwd = *args.Cwd
		}
		res, err := exec.runCommand(ctx, args.Command, argv, cwd)
		return res, nil, err
	})

	return srv
}

// runCommand executes synchronously in the workspace with the run
// environment. A non-zero exit is a tool-level error (isError) carrying the
// exit code + combined output; spawn failures return the Go error.
func (e *Executor) runCommand(ctx context.Context, command string, argv []string, cwd string) (*gmcp.CallToolResult, error) {
	if command == "" {
		return nil, errEmptyCommand
	}
	if cwd == "" {
		cwd = e.Workdir
	}
	cmd := exec.CommandContext(ctx, command, argv...)
	cmd.Dir = cwd
	cmd.Env = e.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return &gmcp.CallToolResult{
				Content: []gmcp.Content{&gmcp.TextContent{Text: "exit " + itoa(ee.ExitCode()) + "\n" + string(out)}},
				IsError: true,
			}, nil
		}
		return nil, err
	}
	return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(out)}}}, nil
}

// HTTPHandler wraps the run's workspace server as a streamable-HTTP handler
// for /mcp/{runID}. Stateless: every request resolves the run's server.
func HTTPHandler(exec *Executor) http.Handler {
	return gmcp.NewStreamableHTTPHandler(func(*http.Request) *gmcp.Server {
		return NewServer(exec)
	}, &gmcp.StreamableHTTPOptions{Stateless: true})
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return ""
	}
	return path[:i]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
