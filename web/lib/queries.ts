"use client";

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  listRuntimes,
  createRuntime,
  deleteRuntime,
  listAgents,
  createAgent,
  deleteAgent,
  listTasks,
  getTask,
  createTask,
  deleteTask,
  assignTask,
  cancelTask,
  waitChildren,
  getTaskMessages,
} from "./api";
import { useWSEvent } from "./ws";

// ── Query keys ──
export const qk = {
  runtimes: ["runtimes"] as const,
  agents: ["agents"] as const,
  tasks: ["tasks"] as const,
  task: (id: string) => ["task", id] as const,
  messages: (id: string) => ["messages", id] as const,
};

// ── Runtime hooks ──
export function useRuntimes() {
  return useQuery({ queryKey: qk.runtimes, queryFn: listRuntimes });
}
export function useCreateRuntime() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createRuntime,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.runtimes }),
  });
}
export function useDeleteRuntime() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteRuntime,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.runtimes }),
  });
}

// ── Agent hooks ──
export function useAgents() {
  return useQuery({ queryKey: qk.agents, queryFn: listAgents });
}
export function useCreateAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createAgent,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.agents }),
  });
}
export function useDeleteAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteAgent,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.agents }),
  });
}

// ── Task hooks ──
export function useTasks() {
  return useQuery({ queryKey: qk.tasks, queryFn: listTasks });
}
export function useTask(id: string) {
  return useQuery({ queryKey: qk.task(id), queryFn: () => getTask(id) });
}
export function useTaskMessages(id: string) {
  return useQuery({ queryKey: qk.messages(id), queryFn: () => getTaskMessages(id) });
}
export function useCreateTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createTask,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.tasks }),
  });
}
export function useDeleteTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteTask,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.tasks }),
  });
}
export function useAssignTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; assignee_type: string; assignee_id: string; handoff_note?: string }) =>
      assignTask(id, body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.tasks });
    },
  });
}
export function useCancelTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: cancelTask,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.tasks }),
  });
}
export function useWaitChildren() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: waitChildren,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.tasks }),
  });
}

// ── WS event → invalidate ──
export function useTaskEvents() {
  const qc = useQueryClient();
  useWSEvent("task:created", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("task:finished", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("task:deleted", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("task:assigned", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("task:retrying", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("task:waiting", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("task:wakeup", () => qc.invalidateQueries({ queryKey: qk.tasks }));
  useWSEvent("agent:created", () => qc.invalidateQueries({ queryKey: qk.agents }));
  useWSEvent("agent:deleted", () => qc.invalidateQueries({ queryKey: qk.agents }));
}
