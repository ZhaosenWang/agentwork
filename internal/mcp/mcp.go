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
//
// Command execution is ASYNC and terminal-shaped (the same model as the ACP
// terminal RPCs): terminal_create starts a command and returns its id
// immediately, terminal_output polls incremental output + exit status,
// terminal_release kills and forgets. A synchronous run_command was retired:
// it hung the HTTP request for the command's whole lifetime, had no
// command-level timeout, and duplicated the terminal engine with a second
// implementation. The terminal tools share the daemon's per-run
// terminalManager — one engine, two channels, the run's cleanup kills both.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/service"
)

// TerminalHost is the command-execution surface the MCP terminal tools use.
// The daemon's terminalManager implements it (the same per-run pool the ACP
// terminal RPCs use — one engine, two channels; the run's cleanup kills
// both). Defined here so mcp stays importable by the daemon (no cycle).
type TerminalHost interface {
	Create(command string, args []string, env []string, cwd string, byteLimit int) (acp.TerminalId, error)
	Output(id acp.TerminalId, cursor *int64) (*acp.TerminalOutputResponse, *int64, int64, error)
	Release(id acp.TerminalId) error
}

// Executor binds the workspace tools to ONE run: the worktree as the
// filesystem basis and the run environment for commands. The daemon builds
// one per run and registers it under the run id.
type Executor struct {
	// Workdir is the run's worktree root — the workspace.
	Workdir string
	// Env is the command environment (platform base + run context, PATH
	// prepended with the agentwork-cli dir).
	Env []string
	// Run context: collaboration tools act on THIS goal as THIS agent.
	GoalID string
	AgentID string
	RunID  string
	// Services for the collaboration tools (comment / assign / list). The
	// daemon injects them — the tools act in-process, no CLI, no HTTP hop.
	// The CLI was the old collaboration channel; an agent had to learn its
	// command syntax (and sometimes read the platform's source to figure it
	// out). MCP tools carry their own schema — zero learning cost.
	commentSvc *service.CommentService
	goalSvc    *service.GoalService
	runSvc     *service.RunService
	agentSvc   *service.AgentService
	squadSvc   *service.SquadService
	// host executes terminal commands — the daemon's per-run terminalManager.
	host TerminalHost
}

// NewExecutor binds an Executor to one run's workspace + terminal pool.
func NewExecutor(workdir string, env []string, host TerminalHost) *Executor {
	return &Executor{Workdir: workdir, Env: env, host: host}
}

// SetCollaboration wires the services the collaboration tools need.
func (e *Executor) SetCollaboration(goalID, agentID, runID string, commentSvc *service.CommentService, goalSvc *service.GoalService, runSvc *service.RunService, agentSvc *service.AgentService, squadSvc *service.SquadService) {
	e.GoalID, e.AgentID, e.RunID = goalID, agentID, runID
	e.commentSvc, e.goalSvc, e.runSvc, e.agentSvc, e.squadSvc = commentSvc, goalSvc, runSvc, agentSvc, squadSvc
}

// NewServer builds the MCP server with the workspace tools registered.
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

	// defaultCreateWait is how long terminal_create waits (synchronously) for
	// a short command before handing the terminal id back: most tool calls are
	// short (ls, git status, one test), and a synchronous result saves the
	// agent the create→poll→release dance. Commands past the budget switch to
	// the async path automatically. The agent controls the command's real
	// lifetime — release (kill) an overlong command; the platform only bounds
	// the turn (run maxRunDuration / idle watchdog) and the concurrent count.
	const defaultCreateWait = 10 * time.Second

	type createArgs struct {
		Command string   `json:"command" jsonschema:"the FINAL executable to run — no shell syntax; pass sh -c \"...\" for shell semantics"`
		Args    *[]string `json:"args,omitempty" jsonschema:"command arguments"`
		Cwd     *string   `json:"cwd,omitempty" jsonschema:"working directory override (defaults to the workspace root)"`
		Timeout *int64    `json:"timeout,omitempty" jsonschema:"sync-wait budget in seconds (default 10): if the command finishes within it the result is returned directly; otherwise a terminal_id comes back and you poll terminal_output. 0 = return the id immediately (pure async)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name: "terminal_create",
		Description: "Start a command on the platform machine with the workspace as the working directory. " +
			"Waits up to the timeout budget (default 10s) for a quick result; commands that finish in time return their " +
			"output and exit status directly (exited=true). Longer commands return a terminal_id with exited=false — " +
			"poll terminal_output (pass the returned cursor back) until exited=true, then terminal_release. " +
			"Commands have no platform time limit of their own: release an overlong one.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args createArgs) (*gmcp.CallToolResult, any, error) {
		if args.Command == "" {
			return nil, nil, errEmptyCommand
		}
		var argv []string
		if args.Args != nil {
			argv = *args.Args
		}
		cwd := exec.Workdir
		if args.Cwd != nil && *args.Cwd != "" {
			cwd = *args.Cwd
		}
		budget := defaultCreateWait
		if args.Timeout != nil {
			if *args.Timeout <= 0 {
				budget = 0 // explicit 0: pure async, return the id immediately
			} else {
				budget = time.Duration(*args.Timeout) * time.Second
			}
		}
		id, err := exec.host.Create(args.Command, argv, exec.Env, cwd, 0)
		if err != nil {
			return nil, nil, err
		}
		var cursor *int64
		var out strings.Builder
		var elapsed int64
		deadline := time.Now().Add(budget)
		for budget > 0 {
			resp, next, el, err := exec.host.Output(id, cursor)
			if err != nil {
				return nil, nil, err
			}
			cursor = next
			elapsed = el
			out.WriteString(resp.Output)
			if resp.ExitStatus != nil {
				return toolResult(map[string]any{
					"terminal_id": string(id), "output": out.String(), "cursor": *next,
					"exited": true, "exit_code": derefInt(resp.ExitStatus.ExitCode),
					"signal": derefStr(resp.ExitStatus.Signal), "elapsed": elapsed,
				}), nil, nil
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Budget exhausted — the command is still running: hand back the id +
		// everything seen so far; the agent continues via terminal_output.
		return toolResult(map[string]any{
			"terminal_id": string(id), "output": out.String(), "cursor": *cursor,
			"exited": false, "elapsed": elapsed,
		}), nil, nil
	})

	type outputArgs struct {
		TerminalID string `json:"terminal_id" jsonschema:"the terminal id from terminal_create"`
		Cursor     *int64 `json:"cursor,omitempty" jsonschema:"opaque cursor from the previous terminal_output call; omit on the first poll"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name: "terminal_output",
		Description: "Poll a command's output: returns the bytes since the given cursor (omit on the first call), " +
			"the next cursor to pass back, and the exit status once finished. Repeat until exited=true, then terminal_release. " +
			"The cursor makes retries safe — re-polling with an old cursor never skips or duplicates bytes.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args outputArgs) (*gmcp.CallToolResult, any, error) {
		resp, next, elapsed, err := exec.host.Output(acp.TerminalId(args.TerminalID), args.Cursor)
		if err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{
			"output": resp.Output, "cursor": *next, "truncated": resp.Truncated,
			"exited": resp.ExitStatus != nil,
			"exit_code": derefInt(exitCode(resp)), "signal": derefStr(exitSignal(resp)),
			"elapsed": elapsed,
		}), nil, nil
	})

	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "terminal_release",
		Description: "Kill the command (if still running) and forget it. Always call after terminal_output reports exited.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args outputArgs) (*gmcp.CallToolResult, any, error) {
		if err := exec.host.Release(acp.TerminalId(args.TerminalID)); err != nil {
			return nil, nil, err
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: "released " + args.TerminalID}}}, nil, nil
	})

	// ── Collaboration tools (决策 4-13): the agent coordinates through MCP
	// tools, not a CLI — structured args, schema-described, no command
	// syntax to learn. The CLI channel is retired from agent guidance.
	type commentArgs struct {
		Content string `json:"content" jsonschema:"the comment text; embed a structured mention like [@Name](mention://agent/<id>) to trigger a run on that agent"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "goal_comment",
		Description: "Post a comment on THIS goal as your agent — the collaboration surface. Embed a mention ([@Name](mention://agent/<uuid>) or mention://squad/<uuid>) to trigger a run on that agent; resolve uuids with agent_list / squad_list. Bare @handle does NOT trigger anything.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args commentArgs) (*gmcp.CallToolResult, any, error) {
		if exec.commentSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if _, err := exec.commentSvc.Create(ctx, service.Comment{
			GoalID: exec.GoalID, AuthorType: "agent", AuthorID: exec.AgentID, Content: args.Content, RunID: exec.RunID,
		}); err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true}), nil, nil
	})

	type assignArgs struct {
		AssigneeType string `json:"assignee_type" jsonschema:"agent | squad | human"`
		AssigneeID   string `json:"assignee_id" jsonschema:"the agent/squad id to hand the goal to ('' with human)"`
		Note         string `json:"note,omitempty" jsonschema:"scoping instruction for the new owner — becomes their next run's scope"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "goal_assign",
		Description: "Hand THIS goal to another agent/squad (or back to the human). END YOUR TURN right after — the platform terminates your run, the new owner's run drives the goal. Include a note that scopes the work for the new owner.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args assignArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		g, err := exec.goalSvc.Assign(ctx, exec.GoalID, args.AssigneeType, args.AssigneeID, args.Note)
		if err != nil {
			return nil, nil, err
		}
		// Enqueue the new owner's run (same as the Web handler): Assign moves
		// ownership; the run drives the goal forward. Coalesces if pending.
		if args.AssigneeType == "agent" || args.AssigneeType == "squad" {
			if _, err := exec.runSvc.EnqueueForGoal(ctx, *g); err != nil {
				return nil, nil, err
			}
		}
		return toolResult(map[string]any{"ok": true}), nil, nil
	})

	type listArgs struct {
		Status *string `json:"status,omitempty" jsonschema:"filter by status (backlog|active|review|done|failed|cancelled)"`
		Limit  *int    `json:"limit,omitempty" jsonschema:"max results (default all)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "goal_list",
		Description: "List goals (optionally filtered) — see what is waiting, what is active, what needs attention.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args listArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		goals, err := exec.goalSvc.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		var out []map[string]any
		for _, g := range goals {
			if args.Status != nil && g.Status != *args.Status {
				continue
			}
			out = append(out, map[string]any{"id": g.ID, "title": g.Title, "status": g.Status, "assignee": g.AssigneeType + "/" + g.AssigneeID})
			if args.Limit != nil && len(out) >= *args.Limit {
				break
			}
		}
		return toolResult(map[string]any{"goals": out}), nil, nil
	})

	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "agent_list",
		Description: "List all agents — resolve agent uuids for mentions (goal_comment) and handoffs (goal_assign).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args struct{}) (*gmcp.CallToolResult, any, error) {
		if exec.agentSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		agents, err := exec.agentSvc.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		var out []map[string]any
		for _, a := range agents {
			out = append(out, map[string]any{"id": a.ID, "name": a.Name, "description": a.Description})
		}
		return toolResult(map[string]any{"agents": out}), nil, nil
	})

	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "squad_list",
		Description: "List all squads — resolve squad uuids for mentions and handoffs.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args struct{}) (*gmcp.CallToolResult, any, error) {
		if exec.squadSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		squads, err := exec.squadSvc.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		var out []map[string]any
		for _, sq := range squads {
			out = append(out, map[string]any{"id": sq.ID, "name": sq.Name, "leader_id": sq.LeaderID})
		}
		return toolResult(map[string]any{"squads": out}), nil, nil
	})

	return srv
}

// errEmptyCommand guards the terminal_create tool against a missing executable.
var errEmptyCommand = errors.New("terminal_create: command is required")

// toolResult marshals the tool's JSON response (the SDK delivers text
// content; a structured string keeps the schema simple).
func toolResult(v map[string]any) *gmcp.CallToolResult {
	raw, _ := json.Marshal(v)
	return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(raw)}}}
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func exitCode(r *acp.TerminalOutputResponse) *int {
	if r.ExitStatus == nil {
		return nil
	}
	return r.ExitStatus.ExitCode
}

func exitSignal(r *acp.TerminalOutputResponse) *string {
	if r.ExitStatus == nil {
		return nil
	}
	return r.ExitStatus.Signal
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
