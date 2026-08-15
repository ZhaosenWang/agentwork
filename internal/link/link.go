// Package link is the JSON-RPC 2.0 link between agentwork (the remote
// CLI/sidecar) and agentwork-daemon (the control plane). One WebSocket
// text frame per message; EITHER side may send requests and
// notifications — the CLI registers and uploads, the daemon dispatches
// runs and pushes config (Phase 2+).
//
// Wire shapes follow the existing ACP client conventions (jsonrpc/id/
// method/params, notifications without id, string request ids).
package link

import "encoding/json"

// Methods. Two surfaces share the JSON-RPC wire:
//
//   - the MACHINE link (/connect, persistent): registration, heartbeat,
//     probe reports, run dispatch, event upload (Phase 2);
//   - the AGENT rpc (/rpc, one-shot per CLI command): the agent's
//     collaboration commands, carrying the per-run token.
const (
	MethodMachineRegister    = "machine.register"
	MethodMachineHeartbeat   = "machine.heartbeat"
	MethodMachineProbeUpdate = "machine.probe_update"

	// daemon → machine (Phase 2)
	MethodRunDispatch = "run.dispatch" // request, ack
	MethodRunCancel   = "run.cancel"   // notification

	// machine → daemon (Phase 2)
	MethodRunClaimed    = "run.claimed"     // request, ack
	MethodRunEventBatch = "run.event_batch" // notification (batched stream events)
	MethodRunFinished   = "run.finished"    // request, ack (daemon runs Finish+reconcile)

	// agent → daemon (/rpc, one-shot; every method carries the per-run
	// token in params)
	MethodGoalComment  = "goal.comment"
	MethodGoalComments = "goal.comments"
	MethodGoalList     = "goal.list"
	MethodAgentList    = "agent.list"
	MethodSquadList    = "squad.list"
	MethodSubGoalList  = "subgoal.list"
	MethodSubGoalCreate = "subgoal.create"
	MethodSubGoalCancel = "subgoal.cancel"
	MethodSubGoalVerify = "subgoal.verify"
	MethodSubGoalGet    = "subgoal.get"
	MethodSubGoalVerifications = "subgoal.verifications"
	MethodChangeList    = "change.list"
	MethodChangeIntegrateBegin  = "change.integrate_begin"
	MethodChangeIntegrateFinish = "change.integrate_finish"
	MethodGoalWait     = "goal.wait"
	MethodGoalStats    = "goal.stats"

	// daemon → machine (Phase 4)
	MethodConfigPush = "config.push" // request, ack
)

// Error codes — the standard JSON-RPC set plus link-specific codes.
const (
	CodeParseError   = -32700
	CodeInvalidReq   = -32600
	CodeMethodNotFnd = -32601
	CodeInvalidParams = -32602
	CodeInternal     = -32603
	// CodeAuthDenied: the register was rejected (bad/missing token).
	CodeAuthDenied = -32001
	// CodeForbidden: the caller is authenticated as the WRONG party — a
	// machine reporting on a run that was dispatched to a different machine.
	CodeForbidden = -32002
)

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// ProbeCLI is one agent CLI found on the machine during probing. The
// daemon turns these into runtime rows (Phase 2); until then they are
// displayed in the machines list.
type ProbeCLI struct {
	Name         string   `json:"name"`                   // claude | opencode | openagent
	Version      string   `json:"version"`                // from the probe command
	ACPSpawn     []string `json:"acp_spawn"`              // how to start it as an ACP stdio server
	SkillsDir    string   `json:"skills_dir,omitempty"`   // where its skills live (~/.claude/skills)
	ProfileFiles []string `json:"profile_files,omitempty"` // CLAUDE.md / AGENTS.md / SOUL.md …
}

// RegisterParams is the machine.register request payload.
type RegisterParams struct {
	MachineID string     `json:"machine_id"`
	Name      string     `json:"name"`
	Hostname  string     `json:"hostname"`
	Version   string     `json:"version"`
	CLIs      []ProbeCLI `json:"clis"`
}

// RegisterResult is the machine.register response payload.
type RegisterResult struct {
	OK bool `json:"ok"`
}

// HeartbeatParams is the machine.heartbeat notification payload.
type HeartbeatParams struct {
	MachineID string `json:"machine_id"`
}

// ProbeUpdateParams is the machine.probe_update request payload (the CLI
// re-probed its environment and reports a fresh CLI list).
type ProbeUpdateParams struct {
	MachineID string     `json:"machine_id"`
	CLIs      []ProbeCLI `json:"clis"`
}

// ── /rpc (agent collaboration, per-run token) ──

// RPCToken identifies the caller: the per-run credential issued at claim.
// Every /rpc method's params embed it; the daemon resolves it to the run's
// (goal, agent, role) identity and NEVER trusts self-reported ids.
type RPCToken struct {
	Token string `json:"token"`
}

// GoalCommentParams posts a comment on the run's goal as the run's agent.
type GoalCommentParams struct {
	RPCToken
	Text     string `json:"text"`
	ParentID string `json:"parent_id,omitempty"`
}

// GoalCommentsParams pulls the run's goal comment feed (the shared
// context) — After = incremental reads from the last seen id.
type GoalCommentsParams struct {
	RPCToken
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// GoalListParams lists goals (the agent resolves ids for mentions).
type GoalListParams struct {
	RPCToken
	Limit  int    `json:"limit,omitempty"`
	Status string `json:"status,omitempty"`
}

// SubGoalCreateParams splits work off the run's goal.
type SubGoalCreateParams struct {
	RPCToken
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	AssigneeID  string `json:"assignee_id"`
	VerifierID  string `json:"verifier_id,omitempty"`
}

// SubGoalVerifyParams issues the verifier's verdict.
type SubGoalVerifyParams struct {
	RPCToken
	SubGoalID string `json:"sub_goal_id"`
	Verdict   string `json:"verdict"` // passed|rejected
	Summary   string `json:"summary,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
}

// SubGoalCancelParams cancels a sub-goal.
type SubGoalCancelParams struct {
	RPCToken
	SubGoalID string `json:"sub_goal_id"`
}

// SubGoalGetParams loads one sub-goal (resume-index expansion).
type SubGoalGetParams struct {
	RPCToken
	SubGoalID string `json:"sub_goal_id"`
}

// SubGoalVerificationsParams lists a sub-goal's verification rounds.
type SubGoalVerificationsParams struct {
	RPCToken
	SubGoalID string `json:"sub_goal_id"`
}

// ChangeIntegrateBeginParams starts an integration: the daemon validates
// and marks the change integrating, returning the head ref — the CLI then
// merges it LOCALLY (its cwd is the worktree) and reports back.
type ChangeIntegrateBeginParams struct {
	RPCToken
	ChangeID string `json:"change_id"`
}

// ChangeIntegrateResult is the begin/finish response.
type ChangeIntegrateResult struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status"` // ready|integrating|integrated|conflict
	HeadRef string `json:"head_ref,omitempty"`
	Note    string `json:"note,omitempty"`
	Output  string `json:"output,omitempty"` // the merge's stderr on conflict
}

// ChangeIntegrateFinishParams reports the local merge outcome.
type ChangeIntegrateFinishParams struct {
	RPCToken
	ChangeID string `json:"change_id"`
	OK       bool   `json:"ok"`
	Output   string `json:"output,omitempty"`
}

// GoalWaitParams blocks the CLI until the goal's sub-goals settle
// (the owner's wait tool — returns current children states).
type GoalWaitParams struct {
	RPCToken
}

// ── run dispatch (machine link, Phase 2) ──

// RunDispatchParams is the daemon → machine dispatch: EVERYTHING the
// executor needs, a readonly snapshot assembled at dispatch time. The
// machine spawns the runtime (ACPSpawn), opens its local workdir, runs
// the prompt, and uploads events/finish — it never queries the platform.
type RunDispatchParams struct {
	RunID     string `json:"run_id"`
	GoalID    string `json:"goal_id"`
	AgentID   string `json:"agent_id"`
	Role      string `json:"role"` // owner|subgoal|consult|review|verify
	SubGoalID string `json:"sub_goal_id,omitempty"`
	Attempt   int    `json:"attempt"`
	Token     string `json:"token"` // per-run credential → AGENTWORK_TOKEN env
	Prompt    string `json:"prompt"` // fixed block + wake line, fully assembled

	// Processor runs (goal-less, platform-internal): the machine works in
	// its proc dir (compile-on-repo gets a detached worktree of
	// origin/<default>; scratch/intake a plain directory) and uploads the
	// artifact files with run.finished.
	Proc         bool     `json:"proc,omitempty"`
	ArtifactFiles []string `json:"artifact_files,omitempty"`

	// Worktree spec: scratch domains derive the local directory layout;
	// git (repo) domains carry the clone config — the machine clones the
	// bare repo, checks out the goal branch, commits, and pushes the
	// branch to the remote as agentwork/<branch> (中转, Phase 3).
	Scratch     bool   `json:"scratch"`
	DomainName  string `json:"domain_name,omitempty"`
	DomainID    string `json:"domain_id,omitempty"`
	GitURL      string `json:"git_url,omitempty"`
	GitCredentials string `json:"git_credentials,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	GitIdentity string `json:"git_identity,omitempty"` // "name <email>" for commits

	// Runtime spawn config (from the machine-owned runtime row).
	ACPSpawn []string          `json:"acp_spawn"`          // argv of the ACP stdio server
	Env      map[string]string `json:"env,omitempty"`      // runtime env + agent env (merged daemon-side)
}

// RunDispatchResult is the run.dispatch ack — the machine accepts or
// rejects (rejecting leaves the run queued for another attempt).
type RunDispatchResult struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// RunCancelParams is the run.cancel notification (daemon → machine): stop
// the in-flight execution of a run.
type RunCancelParams struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason"`
}

// RunClaimedParams is the run.claimed ack (machine → daemon): the machine
// started executing the dispatched run.
type RunClaimedParams struct {
	RunID string `json:"run_id"`
}

// RunEvent is one uploaded execution event. Kind mirrors the proto.Event
// types the daemon already persists; Seq is per-run monotonic — the daemon
// detects gaps and refills from the machine's transcript.
type RunEvent struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"` // message|thought|tool_use|tool_result
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

// RunEventBatchParams is the run.event_batch notification (machine →
// daemon): stream events, batched ~100ms, seq-contiguous.
type RunEventBatchParams struct {
	RunID    string     `json:"run_id"`
	SeqStart int64      `json:"seq_start"`
	Events   []RunEvent `json:"events"`
}

// ── config push (machine link, Phase 4) ──

// SkillFile is one file of a skill package.
type SkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SkillPush is one skill package to install on the machine.
type SkillPush struct {
	Name  string      `json:"name"`
	Files []SkillFile `json:"files"`
}

// ConfigPushParams — the daemon pushes an agent's selected skills to its
// machine (config.push). The machine installs them under
// <skills_dir>/agentwork-<name>/ — namespaced, never touching the user's
// own skills. Idempotent overwrite: register-time full sync replays
// offline edits safely.
type ConfigPushParams struct {
	AgentID string      `json:"agent_id"`
	// SystemPrompt rides as AGENTS.md — the agent-level persona goes through
	// the FILE channel (the runtime's profile resolver loads it natively);
	// the per-run role contract stays in the prompt.
	SystemPrompt string      `json:"system_prompt,omitempty"`
	SkillsDir    string      `json:"skills_dir,omitempty"` // from the machine's probe report; '' = ~/.claude/skills
	Skills       []SkillPush `json:"skills"`
}

// ConfigPushResult is the config.push ack.
type ConfigPushResult struct {
	Written []string `json:"written"`
	Errors  []string `json:"errors,omitempty"`
}

// RunFinishedParams is the run.finished request (machine → daemon): the
// execution reached a terminal state — the daemon runs Finish + reconcile.
type RunFinishedParams struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"` // completed|failed|cancelled
	Summary  string `json:"summary,omitempty"`
	// Token echoes the dispatch's per-run token — the daemon swallows a
	// report whose token doesn't match the run's CURRENT claim (a stashed
	// report from an exec killed before a daemon restart must not cancel
	// the re-dispatched attempt). Empty = legacy CLI, not verifiable.
	Token    string `json:"token,omitempty"`
	Evidence string `json:"evidence,omitempty"` // JSON bundle (scratch runs: '')
	// Artifacts: processor runs' FILE results (checks.json etc.), uploaded
	// from the machine — the platform reads structured side effects, never
	// agent stdout (决策 5.3/§9).
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// rpcMarshal marshals params, normalizing empty slices so they reach the
// wire as [] rather than null (JSON-RPC consumers dislike null params).
func rpcMarshal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
