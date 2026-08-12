import type { Runtime, Agent, Goal, Run, Comment, Squad, SquadMember, Schedule, Domain, Checks, TimelineItem } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "http://localhost:7373";

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`${res.status}: ${text}`);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

// ── Runtime ──
export const listRuntimes = () => api<Runtime[]>("/runtimes");
export const getRuntime = (id: string) => api<Runtime>(`/runtimes/${id}`);
export const createRuntime = (body: Omit<Runtime, "id" | "created_at">) =>
  api<Runtime>("/runtimes", { method: "POST", body: JSON.stringify(body) });
export const deleteRuntime = (id: string) =>
  api<void>(`/runtimes/${id}`, { method: "DELETE" });

// ── Agent ──
export const listAgents = () => api<Agent[]>("/agents");
export const getAgent = (id: string) => api<Agent>(`/agents/${id}`);
export const createAgent = (body: Omit<Agent, "id" | "created_at">) =>
  api<Agent>("/agents", { method: "POST", body: JSON.stringify(body) });
export const updateAgent = (id: string, body: Record<string, unknown>) =>
  api<Agent>(`/agents/${id}`, { method: "PUT", body: JSON.stringify(body) });

export const deleteAgent = (id: string) =>
  api<void>(`/agents/${id}`, { method: "DELETE" });

// ── Goal ──
export const listGoals = () => api<Goal[]>("/goals");
export const getGoal = (id: string) => api<Goal>(`/goals/${id}`);
export const createGoal = (body: {
  title: string;
  description?: string;
  parent_id?: string;
  domain_id?: string;
  assignee_type?: string;
  assignee_id?: string;
  status?: string;
  handoff_note?: string;
  created_by_type?: string;
  created_by_id?: string;
}) => api<Goal>("/goals", { method: "POST", body: JSON.stringify(body) });
export const deleteGoal = (id: string) =>
  api<void>(`/goals/${id}`, { method: "DELETE" });
export const assignGoal = (
  id: string,
  body: { assignee_type: string; assignee_id: string; handoff_note?: string }
) => api<Goal>(`/goals/${id}/assign`, { method: "POST", body: JSON.stringify(body) });
export const cancelGoal = (id: string) =>
  api<Goal>(`/goals/${id}/cancel`, { method: "POST" });
export const waitGoalChildren = (id: string) =>
  api<void>(`/goals/${id}/wait`, { method: "POST" });
export const reopenGoal = (id: string, reason?: string) =>
  api<Goal>(`/goals/${id}/reopen`, { method: "POST", body: JSON.stringify({ reason: reason ?? "" }) });
export const resolveGoalReview = (
  id: string,
  body: { decision: "approve" | "reject" | "redirect"; reason?: string }
) => api<Goal>(`/goals/${id}/review`, { method: "POST", body: JSON.stringify(body) });

// ── Run ──
export const listGoalRuns = (goalId: string) =>
  api<Run[]>(`/goals/${goalId}/runs`);
export interface ChatMessage {
  role: string; // user|assistant|tool|system
  content: string;
  tool_calls: string; // JSON
  created_at: string;
}
export const listGoalRunMessages = (goalId: string, runId: string) =>
  api<ChatMessage[]>(`/goals/${goalId}/runs/${runId}/messages`);

// ── Comment ──
export const listGoalComments = (goalId: string) =>
  api<Comment[]>(`/goals/${goalId}/comments`);
export const listGoalTimeline = (goalId: string) =>
  api<TimelineItem[]>(`/goals/${goalId}/timeline`);
export const createGoalComment = (
  goalId: string,
  body: { author_type: string; author_id: string; content: string; parent_id?: string }
) => api<Comment>(`/goals/${goalId}/comments`, { method: "POST", body: JSON.stringify(body) });

// ── Squad ──
export const listSquads = () => api<Squad[]>("/squads");
export const getSquad = (id: string) => api<Squad>(`/squads/${id}`);
export const createSquad = (body: {
  name: string;
  leader_id: string;
  description?: string;
  instructions?: string;
}) => api<Squad>("/squads", { method: "POST", body: JSON.stringify(body) });
export const deleteSquad = (id: string) =>
  api<void>(`/squads/${id}`, { method: "DELETE" });
export const updateSquad = (
  id: string,
  body: { name: string; description: string; leader_id: string; instructions: string }
) => api<Squad>(`/squads/${id}`, { method: "PUT", body: JSON.stringify(body) });

export const removeSquadMember = (squadId: string, memberId: string) =>
  api<void>(`/squads/${squadId}/members/${memberId}`, { method: "DELETE" });

export const addSquadMember = (
  squadId: string,
  body: { member_type: string; member_id: string; role?: string }
) => api<SquadMember>(`/squads/${squadId}/members`, { method: "POST", body: JSON.stringify(body) });
export const listSquadMembers = (squadId: string) =>
  api<SquadMember[]>(`/squads/${squadId}/members`);

// ── Domain ──
export const listDomains = () => api<Domain[]>("/domains");
export const getDomain = (id: string) => api<Domain>(`/domains/${id}`);
export const createDomain = (body: {
  name: string;
  git_url: string;
  type?: string;
  default_branch?: string;
  git_identity?: string;
  git_credentials?: string;
  policy_text?: string;
  processor_agent_id?: string;
  issue_repo?: string;
  issue_assignee?: string;
  issue_assignee_type?: string;
  issue_provider?: string;
}) => api<Domain>("/domains", { method: "POST", body: JSON.stringify(body) });

// updateDomain edits a domain's mutable config (issue handler etc.).
export const updateDomain = (id: string, body: {
  git_url?: string;
  default_branch?: string;
  git_identity?: string;
  git_credentials?: string;
  issue_repo?: string;
  issue_assignee?: string;
  issue_assignee_type?: string;
  issue_provider?: string;
}) => api<Domain>(`/domains/${id}`, { method: "PUT", body: JSON.stringify(body) });
export const deleteDomain = (id: string) =>
  api<void>(`/domains/${id}`, { method: "DELETE" });
export const stopRun = (goalId: string, runId: string) =>
  api<void>(`/goals/${goalId}/runs/${runId}/stop`, { method: "POST" });

export const compileDomainPolicy = (
  id: string,
  body: { policy_text: string; processor_agent_id: string }
) => api<Run>(`/domains/${id}/compile`, { method: "POST", body: JSON.stringify(body) });
export const freezeDomainChecks = (
  id: string,
  body: { checks: Checks; verification_strength: string }
) => api<Domain>(`/domains/${id}/checks`, { method: "POST", body: JSON.stringify(body) });

// ── Gate health (M2) ──
export const getGateStats = () => api<import("./types").GateStat[]>("/gate-decisions/stats");

// ── IM (Feishu connect) ──
export interface ImStatus {
  status: string; // idle | waiting_qr | waiting_message | connected | failed
  receive_id: string;
  app_id: string;
  error: string;
  qr: { url: string; img_base64: string; expires_at: number };
}
export const getImStatus = () => api<ImStatus>("/im/feishu/status");
export const connectFeishu = () =>
  api<{ qr: { url: string; img_base64: string; expires_at: number }; status: string }>("/im/feishu/connect", { method: "POST" });
export const disconnectFeishu = () =>
  api<void>("/im/feishu/connect", { method: "DELETE" });

// ── Platform settings (M3: IM inbound parser agent + digest time) ──
export interface PlatformSettings {
  intake_agent: string; // agent id: who parses the owner's IM messages
  digest_time: string; // HH:MM local, '' = 09:00 default
  webhook_secret: string; // platform webhook secret, shared across providers ('' = polling only)
}
export const getPlatformSettings = () => api<PlatformSettings>("/settings/platform");
export const savePlatformSettings = (body: PlatformSettings) =>
  api<PlatformSettings>("/settings/platform", { method: "PUT", body: JSON.stringify(body) });

// ── Schedule ──
export const listSchedules = () => api<Schedule[]>("/schedules");
export const getSchedule = (id: string) => api<Schedule>(`/schedules/${id}`);
export const createSchedule = (body: {
  name: string;
  title_template: string;
  description?: string;
  assignee_type?: string;
  assignee_id: string;
  cron_expression: string;
  timezone?: string;
}) => api<Schedule>("/schedules", { method: "POST", body: JSON.stringify(body) });
export const deleteSchedule = (id: string) =>
  api<void>(`/schedules/${id}`, { method: "DELETE" });
export const setScheduleEnabled = (id: string, enabled: boolean) =>
  api<Schedule>(`/schedules/${id}/enabled`, { method: "PUT", body: JSON.stringify({ enabled }) });
