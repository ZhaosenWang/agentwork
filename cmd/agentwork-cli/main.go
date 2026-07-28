// agentwork-cli is the agent-side tool. daemon injects it into the agent
// subprocess's PATH plus AGENTWORK_SERVER_URL / AGENTWORK_TASK_ID /
// AGENTWORK_AGENT_ID env vars, so the agent can call it to produce structured
// side effects (handoff, create sub-task, list tasks, append messages) against
// the agentwork HTTP API. CLI-as-tool, like multica §4.
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
	// defaultServerURL is used when AGENTWORK_SERVER_URL is not set. Matches
	// the agentwork default listen addr.
	defaultServerURL = "http://127.0.0.1:7373"
	// httpTimeout bounds a single CLI→server round trip so an unresponsive
	// daemon can't hang the agent's tool call indefinitely.
	httpTimeout = 10 * time.Second
)

// httpClient is shared across all subcommands.
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
	taskID := os.Getenv("AGENTWORK_TASK_ID")
	agentID := os.Getenv("AGENTWORK_AGENT_ID")

	switch os.Args[1] {
	case "task":
		taskCmd(serverURL, taskID, agentID, os.Args[2:])
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
  task list                              list all tasks (JSON)
  task handoff <to-agent-id> [--note N]  hand off the current task to another agent
  task create --title T [--assignee A] [--parent P] [--status S]
                                          create a sub-task (parent defaults to current task)
  task message [--role R] --text T        append a chat_message to the current task
  task wait                               mark the current task as waiting for its sub-tasks;
                                          the daemon re-runs it once all children finish

Environment (injected by daemon):
  AGENTWORK_SERVER_URL   server base URL (default http://127.0.0.1:7373)
  AGENTWORK_TASK_ID      current task id
  AGENTWORK_AGENT_ID     current agent id`)
}

// ── task subcommand ──

func taskCmd(serverURL, taskID, agentID string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli task <list|handoff|create|message|wait>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL + "/tasks")
	case "handoff":
		taskHandoff(serverURL, taskID, args[1:])
	case "create":
		taskCreate(serverURL, taskID, agentID, args[1:])
	case "message":
		taskMessage(serverURL, taskID, args[1:])
	case "wait":
		taskWait(serverURL, taskID, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown task subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func taskHandoff(serverURL, taskID string, args []string) {
	fs := flag.NewFlagSet("task handoff", flag.ExitOnError)
	note := fs.String("note", "", "handoff note for the next agent")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli task handoff <to-agent-id> [--note N]")
		os.Exit(2)
	}
	if taskID == "" {
		fail("AGENTWORK_TASK_ID not set")
	}
	body := map[string]string{"assignee_type": "agent", "assignee_id": fs.Arg(0), "handoff_note": *note}
	post(serverURL+"/tasks/"+taskID+"/assign", body)
}

func taskCreate(serverURL, taskID, agentID string, args []string) {
	fs := flag.NewFlagSet("task create", flag.ExitOnError)
	title := fs.String("title", "", "task title (required)")
	assignee := fs.String("assignee", "", "assignee agent id (defaults to current agent)")
	parent := fs.String("parent", "", "parent task id (defaults to current task)")
	status := fs.String("status", "queued", "task status")
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
		*parent = taskID // default: sub-task of current task
	}
	body := map[string]string{
		"title":          *title,
		"assignee_type":  "agent",
		"assignee_id":    *assignee,
		"parent_id":      *parent,
		"status":         *status,
		"created_by_type": "agent",
		"created_by_id":   agentID,
	}
	post(serverURL+"/tasks", body)
}

func taskMessage(serverURL, taskID string, args []string) {
	fs := flag.NewFlagSet("task message", flag.ExitOnError)
	role := fs.String("role", "assistant", "message role")
	text := fs.String("text", "", "message text (required)")
	fs.Parse(args)
	if *text == "" {
		fail("--text is required")
	}
	if taskID == "" {
		fail("AGENTWORK_TASK_ID not set")
	}
	body := map[string]string{"role": *role, "text": *text}
	postNoBody(serverURL+"/tasks/"+taskID+"/messages", body)
}

func taskWait(serverURL, taskID string, args []string) {
	if taskID == "" {
		fail("AGENTWORK_TASK_ID not set")
	}
	postNoBody(serverURL+"/tasks/"+taskID+"/wait", nil)
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
	doPost(url, body, true)
}

func postNoBody(url string, body any) {
	doPost(url, body, false)
}

func doPost(url string, body any, printResp bool) {
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
	if printResp {
		io.Copy(os.Stdout, resp.Body)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentwork-cli: "+format+"\n", args...)
	os.Exit(1)
}
