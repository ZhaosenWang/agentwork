// API types — mirror the Go structs in internal/service.

export interface Runtime {
  id: string;
  name: string;
  transport: string; // stdio | ws | tcp
  executable: string;
  args: string[];
  endpoint: string;
  env: Record<string, string>;
  protocol: string;
  created_at: string;
}

export interface Agent {
  id: string;
  name: string;
  description: string;
  runtime_id: string;
  system_prompt: string;
  model: string;
  workdir_base: string;
  env: Record<string, string>;
  max_concurrent: number;
  status: string; // offline | online | crashed
  pid: number;
  created_at: string;
}

export type TaskStatus =
  | "backlog"
  | "queued"
  | "running"
  | "waiting_children"
  | "completed"
  | "failed"
  | "cancelled";

export interface Task {
  id: string;
  title: string;
  description: string;
  parent_id: string;
  assignee_type: string; // agent | human
  assignee_id: string;
  status: TaskStatus;
  handoff_note: string;
  created_by_type: string;
  created_by_id: string;
  created_at: string;
}

export interface ChatMessage {
  id: string;
  task_id: string;
  role: string; // user | assistant | tool | thought | system
  content: string;
  tool_calls: string;
  created_at: string;
}

export interface Schedule {
  id: string;
  name: string;
  title_template: string;
  description: string;
  assignee_id: string;
  cron_expression: string;
  timezone: string;
  enabled: boolean;
  next_run_at: string;
  last_run_at: string;
  created_at: string;
}

// WS event shape from the hub: {"topic":"task:message","payload":{...}}
export interface WSEvent {
  topic: string;
  payload: Record<string, unknown>;
}
