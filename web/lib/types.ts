// API types — mirror the Go structs in internal/service.

export interface Runtime {
  id: string;
  name: string;
  transport: string; // stdio | ws | tcp
  provider: string;  // acp | jsonl | jsonrpc
  executable: string;
  args: string[];
  endpoint: string;
  env: Record<string, string>;
  created_at: string;
}

export interface Agent {
  id: string;
  name: string;
  description: string;
  runtime_id: string;
  system_prompt: string;
  model: string;
  env: Record<string, string>;
  max_concurrent: number;
  created_at: string;
}

export type GoalStatus =
  | "backlog"
  | "active"
  | "blocked"
  | "review"
  | "done"
  | "failed"
  | "cancelled";

export interface Goal {
  id: string;
  title: string;
  description: string;
  parent_id: string;
  domain_id: string;
  assignee_type: string; // agent | squad | human
  assignee_id: string;
  status: GoalStatus;
  handoff_note: string;
  review_request: string;
  human_iterations: number;
  created_by_type: string;
  created_by_id: string;
  created_at: string;
}

export interface GateRule {
  name: string; // merge | guard:<...> | request (M2)
  when: string;
}

export interface Guard {
  type: string; // diff_contains | diff_excludes | coverage_delta
  pattern: string;
  min_delta: number;
}

export interface Checks {
  verify: string[];
  guards: Guard[];
  gates: GateRule[];
}

export interface Domain {
  id: string;
  type: string; // repo (M0)
  name: string;
  git_url: string;
  default_branch: string;
  git_identity: string;
  git_credentials: string;
  policy_text: string;
  checks: Checks;
  verification_strength: string; // strong|medium|weak
  max_run_duration: number;
  verify_timeout: number;
  processor_agent_id: string;
  checks_compiled_at: string;
  metrics_baseline: string;
  created_at: string;
}

export type RunStatus = "queued" | "running" | "completed" | "failed" | "cancelled";

export interface Run {
  id: string;
  goal_id: string;
  agent_id: string;
  run_kind: string; // worker|processor
  domain_id: string;
  prompt: string;
  session_id: string;
  workdir: string;
  status: RunStatus;
  attempt: number;
  result_summary: string;
  evidence: string; // JSON: diff stats + verify output + agent summary
  trigger_comment_id: string;
  is_leader_run: boolean;
  squad_id: string;
  queued_at: string;
  started_at: string;
  finished_at: string;
  created_at: string;
}

export interface Comment {
  id: string;
  goal_id: string;
  author_type: string; // human | agent | system
  author_id: string;
  parent_id: string;
  content: string;
  created_at: string;
}

export interface Squad {
  id: string;
  name: string;
  description: string;
  leader_id: string;
  instructions: string;
  created_at: string;
}

export interface SquadMember {
  id: string;
  squad_id: string;
  member_type: string; // agent | human
  member_id: string;
  role: string;
  created_at: string;
}

export interface Schedule {
  id: string;
  name: string;
  title_template: string;
  description: string;
  assignee_type: string; // agent | squad
  assignee_id: string;
  cron_expression: string;
  timezone: string;
  enabled: boolean;
  next_run_at: string;
  last_run_at: string;
  created_at: string;
}

// WS event shape from the hub: {"topic":"goal:created","payload":{...}}
export type WSTopic =
  | "goal:created" | "goal:assigned" | "goal:finished"
  | "goal:retrying" | "goal:retry_failed" | "goal:waiting" | "goal:deleted"
  | "goal:reviewing" | "goal:approved" | "goal:review_resolved"
  | "goal:delivered" | "goal:deliver_failed"
  | "run:enqueued" | "run:coalesced" | "run:discarded" | "run:event"
  | "comment:created"
  | "agent:created" | "agent:deleted"
  | "squad:created" | "squad:deleted" | "squad:member_added"
  | "schedule:created" | "schedule:fired"
  | "domain:created" | "domain:deleted" | "domain:compiled" | "domain:compile_failed";

export interface WSEvent {
  topic: WSTopic;
  payload: Record<string, unknown>;
}
