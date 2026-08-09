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
  goal list [--limit N]                    list goals (JSON); --limit caps to N most recent (default all)
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

// goalList implements `goal list [--limit N]`. --limit truncates to the N
// most recent goals; absent (or 0) means all.
func goalList(serverURL string, args []string) {
	fs := flag.NewFlagSet("goal list", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max number of goals to return (0 = all)")
	fs.Parse(args)
	get(goalListURL(serverURL, *limit))
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
