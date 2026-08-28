// agentwork-cli is the agent-side tool. daemon injects it into the agent
// subprocess's PATH plus AGENTWORK_SERVER_URL / AGENTWORK_GOAL_ID /
// AGENTWORK_RUN_ID / AGENTWORK_AGENT_ID env vars, so the agent can call it to
// produce structured side effects (assign/handoff, create sub-goal, comment,
// wait-children) against the agentwork HTTP API. CLI-as-tool, like multica §4.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/link"
)

const (
	// defaultServerURL matches the agentwork default listen addr.
	defaultServerURL = "http://127.0.0.1:7373"
	httpTimeout      = 10 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

// serverURL resolves the daemon's HTTP base address the SAME way connect
// resolves its WebSocket address: the AGENTWORK_SERVER_URL env var (injected
// by the executor at spawn) with the default listen addr fallback. Every
// command reads it here — no caller threads a serverURL parameter through
// the dispatch chain (the agent CLI and the human debugging CLI share one
// binary and one resolution path).
func serverURL() string {
	if u := os.Getenv("AGENTWORK_SERVER_URL"); u != "" {
		return u
	}
	return defaultServerURL
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	goalID := os.Getenv("AGENTWORK_GOAL_ID")
	agentID := os.Getenv("AGENTWORK_AGENT_ID")

	switch os.Args[1] {
	case "connect":
		connectCmd(os.Args[2:])
		return
	case "status":
		statusCmd(os.Args[2:])
		return
	case "goal":
		goalCmd(goalID, agentID, os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "squad":
		squadCmd(os.Args[2:])
	case "stats":
		statsCmd(os.Args[2:])
	case "subgoal":
		subgoalCmd(os.Args[2:])
	case "change":
		changeCmd(os.Args[2:])
	case "issue":
		issueCmd(goalID, os.Args[2:])
	case "version":
		versionCmd()
		return
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// issueCmd lets the agent reply to the issue behind its current goal (M4-B):
// the platform owns the GitHub token and executes the comment — the agent
// only produces the structured side effect.
func issueCmd(goalID string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli issue comment --text \"...\"")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("issue comment", flag.ExitOnError)
	text := fs.String("text", "", "comment body")
	_ = fs.Parse(args)
	if *text == "" {
		fail("--text is required")
	}
	if goalID == "" {
		fail("AGENTWORK_GOAL_ID not set — this command must run inside a goal's run")
	}
	body, err := json.Marshal(map[string]string{"goal_id": goalID, "text": *text})
	if err != nil {
		fail(err.Error())
	}
	resp, err := http.Post(serverURL()+"/issue-comments", "application/json", strings.NewReader(string(body)))
	if err != nil {
		fail("issue comment: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		fail(fmt.Sprintf("issue comment failed: %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}
	fmt.Println("issue comment posted")
}

func usage() {
	fmt.Fprintln(os.Stderr, `agentwork — agent-side tool (called by agents during task execution)
and remote-machine sidecar (register this host to agentwork-daemon).

Subcommands:
  connect [--server URL] [--token T] [--name N] [--scan DIR|GLOB ...] [--agents FILE]
                                             connect this machine to agentwork-daemon (default
                                             127.0.0.1:7373, no auth), probe its agent CLIs and
                                             register them; --scan adds a directory to search for
                                             agent CLIs not on PATH (repeatable; ~ expanded);
                                             --agents points to a YAML config (default
                                             ~/.agentwork/agents.yaml) whose entries ADD new agent
                                             CLIs or OVERRIDE builtins by name; heartbeats until
                                             interrupted
  status                                     show the persisted connection state (machine id,
                                             server, last heartbeat, probed agent CLIs)
  goal list [--limit N] [--status S] [--json]  list goals (JSON — the default format; --json requests
                                             it explicitly); --limit caps to N most recent (default all);
                                             --status keeps only goals whose status equals S (exact match)
  goal assign <to-agent-id> [--note N]       hand off the current goal to another agent
  goal create --title T [--description D] [--assignee A] [--status S]
                                             create a goal
  goal comment --text T [--role R]           post a comment on the current goal; --text may
                                             contain a structured mention [@Name](mention://agent/<id>)
                                             to enqueue a run on that agent
                                             human to decide (behavior gate)
  agent list                                 list all agents (JSON)
  agent history [--limit N] [--status S]    your recent runs joined to their goals
                                             [--agent ID]                 (JSON; default agent = AGENTWORK_AGENT_ID)
  squad list                                 list all squads (JSON)
  subgoal list|get <id>|create --title T --assignee A [--description D] [--verifier V]
                                             list/create/read work items (the owner splits)
  subgoal cancel <id>                        cancel a work item
  subgoal verify <id> --verdict passed|rejected [--summary S] [--evidence E]
                                             the verifier's verdict
  subgoal verifications <id>                 a work item's verification rounds
  change list                                the goal's changes (ready/integrating/…)
  change integrate <id>                      merge a change into THIS worktree locally;
                                             conflict → the assignee is woken to rework
  stats                                      goal/run status statistics (JSON): goal totals
                                             + counts per status (backlog/active/blocked/done/
                                             failed/cancelled) and run totals + counts per status
                                             (queued/running/completed/failed/cancelled)
  issue comment --text T                     reply to the issue behind the current goal
                                             (the platform owns the token; only for
                                             issue-sourced goals, M4-B)

  version                                    print the CLI build version and exit

Environment (injected by daemon):
  AGENTWORK_SERVER_URL   server base URL (default http://127.0.0.1:7373)
  AGENTWORK_GOAL_ID      current goal id (product plane)
  AGENTWORK_RUN_ID       current run id (execution plane)
  AGENTWORK_AGENT_ID     current agent id`)
}

// ── goal ──

func goalCmd(goalID, agentID string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli goal <list|assign|create|comment|wait>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		goalList(args[1:])
	case "assign":
		goalAssign(goalID, args[1:])
	case "create":
		goalCreate(goalID, agentID, args[1:])
	case "comment":
		goalComment(goalID, args[1:])
	case "comments":
		goalComments(goalID, args[1:])
	case "wait":
		goalWait(goalID, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown goal subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// goalList implements `goal list [--limit N] [--status S] [--json]`. --limit
// truncates to the N most recent goals; absent (or 0) means all. --status
// filters to goals whose status exactly matches S. The server's /goals
// endpoint only supports ?limit, so the status filter is applied
// client-side, and --limit then truncates the filtered list to the N most
// recent matches. Output is JSON — the CLI's native format (agents parse
// stdout), so it is also the default; --json is the explicit selector for
// that format.
func goalList(args []string) {
	fs := flag.NewFlagSet("goal list", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max number of goals to return (0 = all)")
	status := fs.String("status", "", "only list goals with this status (exact match)")
	jsonOut := fs.Bool("json", false, "output goals as JSON (the default output format)")
	fs.Parse(args)
	// JSON is both the default and the only format; --json pins it
	// explicitly for scripted callers. GET /goals always responds with
	// JSON, so the body is streamed through unchanged.
	_ = jsonOut
	if *status == "" {
		get(goalListURL(*limit))
		return
	}
	var goals []json.RawMessage
	if err := getJSON(goalListURL(0), &goals); err != nil {
		fail("%v", err)
	}
	out := filterGoalsByStatus(goals, *status)
	if *limit > 0 && len(out) > *limit {
		out = out[:*limit]
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fail("encode: %v", err)
	}
}

// filterGoalsByStatus returns the goals whose status equals want, preserving
// order. The status is probed via a minimal unmarshal so each goal object
// passes through byte-for-byte (no field loss, no key reordering).
func filterGoalsByStatus(goals []json.RawMessage, want string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(goals))
	for _, g := range goals {
		var probe struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(g, &probe); err != nil {
			continue // malformed goal object: skip
		}
		if probe.Status == want {
			out = append(out, g)
		}
	}
	return out
}

// goalListURL builds the GET /goals URL, appending ?limit=N when N > 0.
func goalListURL(limit int) string {
	url := serverURL() + "/goals"
	if limit > 0 {
		url += fmt.Sprintf("?limit=%d", limit)
	}
	return url
}

// goalAssign hands the current goal off to another agent via /rpc — the
// per-run token (env AGENTWORK_TOKEN) is the identity, resolved server-side
// to the actor. The HTTP /goals/{id}/assign surface carries no agent
// identity (it is the human's action), so an agent handoff over HTTP lost
// its actor and the service-layer owner check was skipped (any agent could
// grab any goal). Over /rpc the token anchors the actor and Assign enforces
// "only the current owner can hand off".
func goalAssign(goalID string, args []string) {
	fs := flag.NewFlagSet("goal assign", flag.ExitOnError)
	note := fs.String("note", "", "handoff note for the next agent")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli goal assign <to-agent-id> [--note N]")
		os.Exit(2)
	}
	if goalID == "" {
		fail("AGENTWORK_GOAL_ID not set")
	}
	var out map[string]any
	if err := rpcCall(link.MethodGoalAssign, link.GoalAssignParams{
		RPCToken:     rpcToken(),
		AssigneeType: "agent",
		AssigneeID:   fs.Arg(0),
		HandoffNote:  *note,
	}, &out); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(out)
}

func goalCreate(goalID, agentID string, args []string) {
	fs := flag.NewFlagSet("goal create", flag.ExitOnError)
	title := fs.String("title", "", "goal title (required)")
	description := fs.String("description", "", "goal description (the work to do)")
	assignee := fs.String("assignee", "", "assignee agent id (defaults to current agent)")
	// Sub-goals are SLEEVED (DESIGN.md 决策 3-6): creation no longer
	// defaults to a child of the current goal — an agent-created goal is an
	// independent item, not a fan-out child (a defaulted parent used to make
	// every agent-created goal a sub-goal that blocks its parent).
	parent := fs.String("parent", "", "parent goal id (explicit sub-goal; NOT defaulted)")
	status := fs.String("status", "active", "goal status")
	fs.Parse(args)
	if *title == "" {
		fail("--title is required")
	}
	if *assignee == "" {
		*assignee = agentID
	}
	if *assignee == "" {
		fail("--assignee is required (or AGENTWORK_AGENT_ID must be set)")
	}
	body := map[string]string{
		"title":           *title,
		"description":     *description,
		"assignee_type":   "agent",
		"assignee_id":     *assignee,
		"parent_id":       *parent,
		"status":          *status,
		"created_by_type": "agent",
		"created_by_id":   agentID,
	}
	post(serverURL()+"/goals", body)
}

// goalComment posts a comment on the run's goal via /rpc — the per-run
// token (env AGENTWORK_TOKEN) is the identity; the daemon resolves it to
// the run's goal and agent.
func goalComment(goalID string, args []string) {
	fs := flag.NewFlagSet("goal comment", flag.ExitOnError)
	text := fs.String("text", "", "comment text (required; may contain a structured mention)")
	parent := fs.String("parent", "", "parent comment id (optional — replies thread under it)")
	ask := fs.Bool("ask", false, "this comment is a question to the human (goal creator) — the platform notifies them and their reply wakes you (决策 7-3)")
	fs.Parse(args)
	if *text == "" {
		fail("--text is required")
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := rpcCall(link.MethodGoalComment, link.GoalCommentParams{
		RPCToken: rpcToken(),
		Text:     *text,
		ParentID: *parent,
		AskHuman: *ask,
	}, &res); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(res)
}

// goalComments pulls the run's goal comment feed via /rpc — the shared
// context. --after reads incrementally from the last seen comment id.
func goalComments(goalID string, args []string) {
	fs := flag.NewFlagSet("goal comments", flag.ExitOnError)
	after := fs.String("after", "", "only comments after this id (incremental read)")
	limit := fs.Int("limit", 50, "max comments to return")
	fs.Parse(args)
	var out []map[string]any
	if err := rpcCall(link.MethodGoalComments, link.GoalCommentsParams{
		RPCToken: rpcToken(),
		After:    *after,
		Limit:    *limit,
	}, &out); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(out)
}

// goalWait parks until the goal's sub-goals settle (or the server-side
// timeout) via /rpc, then prints their states.
func goalWait(goalID string, args []string) {
	var states []map[string]any
	if err := rpcCall(link.MethodGoalWait, link.GoalWaitParams{RPCToken: rpcToken()}, &states); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(states)
}


// ── agent / squad ──

func agentCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli agent <list|history>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/agents")
	case "history":
		agentHistory(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// agentHistory implements `agent history [--limit N] [--status S] [--agent ID]`.
// It returns the calling agent's recent runs joined to their goals — the chat
// surface's "what have I done" view. The agent id defaults to
// AGENTWORK_AGENT_ID (set by the executor at spawn, both run and chat paths);
// --agent overrides it for manual invocation. Output is JSON (agents parse
// stdout), matching `goal list`.
func agentHistory(args []string) {
	fs := flag.NewFlagSet("agent history", flag.ExitOnError)
	limit := fs.Int("limit", 20, "max number of runs to return")
	status := fs.String("status", "", "only runs with this status (exact match)")
	agentID := fs.String("agent", "", "agent id (default: AGENTWORK_AGENT_ID)")
	fs.Parse(args)
	id := *agentID
	if id == "" {
		id = os.Getenv("AGENTWORK_AGENT_ID")
	}
	if id == "" {
		fail("agent id is required: pass --agent or set AGENTWORK_AGENT_ID")
	}
	u := serverURL() + "/agents/" + id + "/history?limit=" + strconv.Itoa(*limit)
	if *status != "" {
		u += "&status=" + url.QueryEscape(*status)
	}
	get(u)
}

func squadCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli squad <list>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/squads")
	default:
		fmt.Fprintf(os.Stderr, "unknown squad subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// ── stats ──

// cliGoal is the minimal goal shape `stats` needs from GET /goals.
type cliGoal struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// cliRun is the minimal run shape `stats` needs from GET /goals/{id}/runs.
type cliRun struct {
	Status string `json:"status"`
}

// knownGoalStatuses / knownRunStatuses are the status dimensions `stats`
// reports. Every known status is always present in the output (zero-filled),
// so the JSON is self-describing; unknown statuses the server may return are
// still counted under their own key.
var (
	knownGoalStatuses = []string{"backlog", "active", "blocked", "done", "failed", "cancelled"}
	knownRunStatuses  = []string{"queued", "running", "completed", "failed", "cancelled"}
)

// statusBucket is a total plus per-status counts for one dimension.
type statusBucket struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
}

// statsOutput is the JSON shape emitted by `stats`.
type statsOutput struct {
	Goals statusBucket `json:"goals"`
	Runs  statusBucket `json:"runs"`
}

// newStatusBucket initializes a bucket with zero counts for every known status.
func newStatusBucket(known []string) statusBucket {
	byStatus := make(map[string]int, len(known))
	for _, s := range known {
		byStatus[s] = 0
	}
	return statusBucket{ByStatus: byStatus}
}

// bucketGoals tallies a goal list into a statusBucket.
func bucketGoals(goals []cliGoal) statusBucket {
	b := newStatusBucket(knownGoalStatuses)
	b.Total = len(goals)
	for _, g := range goals {
		b.ByStatus[g.Status]++
	}
	return b
}

// bucketRuns tallies a run list into a statusBucket.
func bucketRuns(runs []cliRun) statusBucket {
	b := newStatusBucket(knownRunStatuses)
	b.Total = len(runs)
	for _, r := range runs {
		b.ByStatus[r.Status]++
	}
	return b
}

// runsListURL builds the GET /goals/{id}/runs URL.
func runsListURL(goalID string) string {
	return serverURL() + "/goals/" + goalID + "/runs"
}

// statsCmd implements `stats`: goal stats come from GET /goals; run stats are
// aggregated by fanning out to GET /goals/{id}/runs for every goal and
// summing the per-status counts. Output is a single JSON object, matching the
// JSON output style of the other commands.
func statsCmd(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	fs.Parse(args)

	var goals []cliGoal
	if err := getJSON(goalListURL(0), &goals); err != nil {
		fail("%v", err)
	}

	var allRuns []cliRun
	for _, g := range goals {
		var runs []cliRun
		if err := getJSON(runsListURL(g.ID), &runs); err != nil {
			fail("%v", err)
		}
		allRuns = append(allRuns, runs...)
	}

	out := statsOutput{Goals: bucketGoals(goals), Runs: bucketRuns(allRuns)}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fail("encode stats: %v", err)
	}
}

// ── HTTP helpers ──

func get(url string) {
	resp, err := httpClient.Get(url)
	if err != nil {
		fail("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fail("GET %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	io.Copy(os.Stdout, resp.Body)
}

// getJSON performs GET and decodes the JSON body into v. Mirrors get's
// failure mode: transport errors and non-2xx responses are returned as
// errors (the caller decides how to surface them).
func getJSON(url string, v any) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s: decode: %v", url, err)
	}
	return nil
}

func post(url string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		fail("marshal: %v", err)
	}
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		fail("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		fail("POST %s: HTTP %d: %s", url, resp.StatusCode, rb)
	}
	io.Copy(os.Stdout, resp.Body)
}

func postNoBody(url string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		fail("marshal: %v", err)
	}
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		fail("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		fail("POST %s: HTTP %d: %s", url, resp.StatusCode, rb)
	}
}

// versionCmd prints just the build version (e.g. "v0.0.2") on stdout — the
// machine-capture form (VER=$(agentwork version)). The binary name is dropped
// so the output is the version alone, matching `git --version`/`go version`
// convention (stdout, not stderr).
func versionCmd() {
	fmt.Printf("v%s\n", cliVersion)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentwork-cli: "+format+"\n", args...)
	os.Exit(1)
}
