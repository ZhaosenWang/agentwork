package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/eushing/agentwork/internal/link"
)

// collab — the CLI's sub-goal and change commands (CLI 分支 ②): the full
// MCP-collaboration parity over /rpc. change integrate merges LOCALLY (the
// agent's terminal cwd IS the worktree — on the machine or the daemon host
// alike); the platform only records the state transitions.

func subgoalCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork subgoal <list|get|create|cancel|retry|verify|verifications>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		var out []map[string]any
		if err := rpcCall(link.MethodSubGoalList, link.RPCToken(rpcToken()), &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "get":
		if len(args) < 2 {
			fail("usage: agentwork subgoal get <sub-goal-id>")
		}
		var out map[string]any
		if err := rpcCall(link.MethodSubGoalGet, link.SubGoalGetParams{RPCToken: rpcToken(), SubGoalID: args[1]}, &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "create":
		fs := flag.NewFlagSet("subgoal create", flag.ExitOnError)
		title := fs.String("title", "", "work item title (required)")
		desc := fs.String("description", "", "work item description")
		assignee := fs.String("assignee", "", "assignee agent id (required)")
		verifier := fs.String("verifier", "", "verifier agent id ('' = machine verification)")
		_ = fs.Parse(args[1:])
		if *title == "" || *assignee == "" {
			fail("--title and --assignee are required")
		}
		var out map[string]any
		if err := rpcCall(link.MethodSubGoalCreate, link.SubGoalCreateParams{
			RPCToken: rpcToken(), Title: *title, Description: *desc, AssigneeID: *assignee, VerifierID: *verifier,
		}, &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "cancel":
		if len(args) < 2 {
			fail("usage: agentwork subgoal cancel <sub-goal-id>")
		}
		var out map[string]any
		if err := rpcCall(link.MethodSubGoalCancel, link.SubGoalCancelParams{RPCToken: rpcToken(), SubGoalID: args[1]}, &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "retry":
		if len(args) < 2 {
			fail("usage: agentwork subgoal retry <sub-goal-id>")
		}
		var out map[string]any
		if err := rpcCall(link.MethodSubGoalRetry, link.SubGoalRetryParams{RPCToken: rpcToken(), SubGoalID: args[1]}, &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "verify":
		fs := flag.NewFlagSet("subgoal verify", flag.ExitOnError)
		verdict := fs.String("verdict", "", "passed|rejected (required)")
		summary := fs.String("summary", "", "verdict summary")
		evidence := fs.String("evidence", "", "evidence (command output etc.)")
		_ = fs.Parse(args[1:])
		if fs.NArg() < 1 || (*verdict != "passed" && *verdict != "rejected") {
			fail("usage: agentwork subgoal verify <sub-goal-id> --verdict passed|rejected")
		}
		var out map[string]any
		if err := rpcCall(link.MethodSubGoalVerify, link.SubGoalVerifyParams{
			RPCToken: rpcToken(), SubGoalID: fs.Arg(0), Verdict: *verdict, Summary: *summary, Evidence: *evidence,
		}, &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "verifications":
		if len(args) < 2 {
			fail("usage: agentwork subgoal verifications <sub-goal-id>")
		}
		var out []map[string]any
		if err := rpcCall(link.MethodSubGoalVerifications, link.SubGoalVerificationsParams{RPCToken: rpcToken(), SubGoalID: args[1]}, &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	default:
		fmt.Fprintf(os.Stderr, "unknown subgoal subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func changeCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork change <list|integrate>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		var out []map[string]any
		if err := rpcCall(link.MethodChangeList, link.RPCToken(rpcToken()), &out); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(out)
	case "integrate":
		if len(args) < 2 {
			fail("usage: agentwork change integrate <change-id>")
		}
		changeIntegrate(args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown change subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// changeIntegrate runs the integration: the platform validates + marks the
// change integrating (returning the head ref), the CLI merges it LOCALLY
// (cwd = the worktree — on the machine or the daemon host alike), and the
// platform records the outcome (conflict wakes the assignee to rework).
func changeIntegrate(changeID string) {
	var begun link.ChangeIntegrateResult
	if err := rpcCall(link.MethodChangeIntegrateBegin, link.ChangeIntegrateBeginParams{RPCToken: rpcToken(), ChangeID: changeID}, &begun); err != nil {
		fail("%v", err)
	}
	if !begun.OK {
		rpcPrintJSON(begun)
		return
	}
	cwd, _ := os.Getwd()
	// The change head lives on the remote (pushed by the assignee's run) —
	// refresh origin first or the merge fails on a stale clone and is
	// misreported as a conflict (waking the assignee for nothing).
	fetch := exec.Command("git", "fetch", "origin")
	fetch.Dir = cwd
	_ = fetch.Run() // best-effort: a scratch/offline workdir may have no origin
	cmd := exec.Command("git", "merge", "--no-ff", begun.HeadRef, "-m", "Integrate "+changeID)
	cmd.Dir = cwd
	out, mergeErr := cmd.CombinedOutput()
	if mergeErr != nil {
		// One retry: the change head may not be reachable from the default
		// refspec — ask the remote for the object itself.
		fetchSha := exec.Command("git", "fetch", "origin", begun.HeadRef)
		fetchSha.Dir = cwd
		if fetchSha.Run() == nil {
			retry := exec.Command("git", "merge", "--no-ff", begun.HeadRef, "-m", "Integrate "+changeID)
			retry.Dir = cwd
			if retryOut, rerr := retry.CombinedOutput(); rerr == nil {
				out, mergeErr = retryOut, nil
			}
		}
	}
	if mergeErr != nil {
		// Conflict: abort in the worktree, report — the platform marks the
		// change conflicted and wakes the assignee.
		abort := exec.Command("git", "merge", "--abort")
		abort.Dir = cwd
		_ = abort.Run()
		var res link.ChangeIntegrateResult
		if err := rpcCall(link.MethodChangeIntegrateFinish, link.ChangeIntegrateFinishParams{
			RPCToken: rpcToken(), ChangeID: changeID, OK: false, Output: strings.TrimSpace(string(out)),
		}, &res); err != nil {
			fail("%v", err)
		}
		rpcPrintJSON(res)
		return
	}
	var res link.ChangeIntegrateResult
	if err := rpcCall(link.MethodChangeIntegrateFinish, link.ChangeIntegrateFinishParams{
		RPCToken: rpcToken(), ChangeID: changeID, OK: true,
	}, &res); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(res)
}
