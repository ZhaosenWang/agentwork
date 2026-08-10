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
	"os"
	"time"
)

const (
	// defaultServerURL matches the agentwork default listen addr.
	defaultServerURL = "http://127.0.0.1:7373"
	httpTimeout      = 10 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	serverURL := os.Getenv("AGENTWORK_SERVER_URL")
	if serverURL == "" {
		serverURL = defaultServerURL
	}
	goalID := os.Getenv("AGENTWORK_GOAL_ID")
	agentID := os.Getenv("AGENTWORK_AGENT_ID")

	switch os.Args[1] {
	case "goal":
		goalCmd(serverURL, goalID, agentID, os.Args[2:])
	case "agent":
		agentCmd(serverURL, os.Args[2:])
	case "squad":
		squadCmd(serverURL, os.Args[2:])
	case "stats":
		statsCmd(serverURL, os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `agentwork-cli — agent-side tool (called by agents during task execution)

Subcommands:
  goal list [--limit N] [--status S] [--json]  list goals (JSON — the default format; --json requests
                                             it explicitly); --limit caps to N most recent (default all);
                                             --status keeps only goals whose status equals S (exact match)
  goal assign <to-agent-id> [--note N]       hand off the current goal to another agent
  goal create --title T [--description D] [--assignee A] [--parent P] [--status S]
                                             create a sub-goal (parent defaults to current goal)
  goal comment --text T [--role R]           post a comment on the current goal; --text may
                                             contain a structured mention [@Name](mention://agent/<id>)
                                             to enqueue a run on that agent
  goal wait                                  mark the current goal as waiting for its sub-goals;
                                             the daemon re-runs it once all children finish
  agent list                                 list all agents (JSON)
  squad list                                 list all squads (JSON)
  stats                                      goal/run status statistics (JSON): goal totals
                                             + counts per status (backlog/active/blocked/done/
                                             failed/cancelled) and run totals + counts per status
                                             (queued/running/completed/failed/cancelled)

Environment (injected by daemon):
  AGENTWORK_SERVER_URL   server base URL (default http://127.0.0.1:7373)
  AGENTWORK_GOAL_ID      current goal id (product plane)
  AGENTWORK_RUN_ID       current run id (execution plane)
  AGENTWORK_AGENT_ID     current agent id`)
}

// ── goal ──

func goalCmd(serverURL, goalID, agentID string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli goal <list|assign|create|comment|wait>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		goalList(serverURL, args[1:])
	case "assign":
		goalAssign(serverURL, goalID, args[1:])
	case "create":
		goalCreate(serverURL, goalID, agentID, args[1:])
	case "comment":
		goalComment(serverURL, goalID, args[1:])
	case "wait":
		goalWait(serverURL, goalID, args[1:])
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
func goalList(serverURL string, args []string) {
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
		get(goalListURL(serverURL, *limit))
		return
	}
	var goals []json.RawMessage
	if err := getJSON(goalListURL(serverURL, 0), &goals); err != nil {
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
func goalListURL(serverURL string, limit int) string {
	url := serverURL + "/goals"
	if limit > 0 {
		url += fmt.Sprintf("?limit=%d", limit)
	}
	return url
}

func goalAssign(serverURL, goalID string, args []string) {
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
	body := map[string]string{"assignee_type": "agent", "assignee_id": fs.Arg(0), "handoff_note": *note}
	post(serverURL+"/goals/"+goalID+"/assign", body)
}

func goalCreate(serverURL, goalID, agentID string, args []string) {
	fs := flag.NewFlagSet("goal create", flag.ExitOnError)
	title := fs.String("title", "", "goal title (required)")
	description := fs.String("description", "", "goal description (the work to do)")
	assignee := fs.String("assignee", "", "assignee agent id (defaults to current agent)")
	parent := fs.String("parent", "", "parent goal id (defaults to current goal)")
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
	if *parent == "" {
		*parent = goalID
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
	post(serverURL+"/goals", body)
}

func goalComment(serverURL, goalID string, args []string) {
	fs := flag.NewFlagSet("goal comment", flag.ExitOnError)
	role := fs.String("role", "agent", "author role (agent|human|system)")
	text := fs.String("text", "", "comment text (required; may contain a structured mention)")
	fs.Parse(args)
	if *text == "" {
		fail("--text is required")
	}
	if goalID == "" {
		fail("AGENTWORK_GOAL_ID not set")
	}
	body := map[string]string{"author_type": *role, "author_id": os.Getenv("AGENTWORK_AGENT_ID"), "content": *text}
	post(serverURL+"/goals/"+goalID+"/comments", body)
}

func goalWait(serverURL, goalID string, args []string) {
	if goalID == "" {
		fail("AGENTWORK_GOAL_ID not set")
	}
	postNoBody(serverURL+"/goals/"+goalID+"/wait", nil)
}

// ── agent / squad ──

func agentCmd(serverURL string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli agent <list>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL + "/agents")
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func squadCmd(serverURL string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli squad <list>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL + "/squads")
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
func runsListURL(serverURL, goalID string) string {
	return serverURL + "/goals/" + goalID + "/runs"
}

// statsCmd implements `stats`: goal stats come from GET /goals; run stats are
// aggregated by fanning out to GET /goals/{id}/runs for every goal and
// summing the per-status counts. Output is a single JSON object, matching the
// JSON output style of the other commands.
func statsCmd(serverURL string, args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	fs.Parse(args)

	var goals []cliGoal
	if err := getJSON(goalListURL(serverURL, 0), &goals); err != nil {
		fail("%v", err)
	}

	var allRuns []cliRun
	for _, g := range goals {
		var runs []cliRun
		if err := getJSON(runsListURL(serverURL, g.ID), &runs); err != nil {
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

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentwork-cli: "+format+"\n", args...)
	os.Exit(1)
}
