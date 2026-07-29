import type { Runtime, Agent, Task, ChatMessage } from "./types";

const API_BASE = "http://localhost:7373";

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
export const createRuntime = (body: Omit<Runtime, "id" | "created_at">) =>
  api<Runtime>("/runtimes", { method: "POST", body: JSON.stringify(body) });
export const deleteRuntime = (id: string) =>
  api<void>(`/runtimes/${id}`, { method: "DELETE" });

// ── Agent ──
export const listAgents = () => api<Agent[]>("/agents");
export const createAgent = (body: Omit<Agent, "id" | "status" | "pid" | "created_at">) =>
  api<Agent>("/agents", { method: "POST", body: JSON.stringify(body) });
export const deleteAgent = (id: string) =>
  api<void>(`/agents/${id}`, { method: "DELETE" });

// ── Task ──
export const listTasks = () => api<Task[]>("/tasks");
export const getTask = (id: string) => api<Task>(`/tasks/${id}`);
export const createTask = (body: Partial<Task>) =>
  api<Task>("/tasks", { method: "POST", body: JSON.stringify(body) });
export const deleteTask = (id: string) =>
  api<void>(`/tasks/${id}`, { method: "DELETE" });
export const assignTask = (id: string, body: { assignee_type: string; assignee_id: string; handoff_note?: string }) =>
  api<Task>(`/tasks/${id}/assign`, { method: "POST", body: JSON.stringify(body) });
export const cancelTask = (id: string) =>
  api<Task>(`/tasks/${id}/cancel`, { method: "POST" });
export const waitChildren = (id: string) =>
  api<void>(`/tasks/${id}/wait`, { method: "POST" });

// ── Chat messages ──
// Backend returns null (200) when the task has no messages yet; normalize
// to [] so downstream .map() is always safe. A 404 also falls back to [].
export const getTaskMessages = (id: string) =>
  api<ChatMessage[] | null>(`/tasks/${id}/messages`)
    .then((r) => r ?? [])
    .catch(() => [] as ChatMessage[]);
