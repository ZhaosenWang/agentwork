"use client";

import Link from "next/link";
import { useState } from "react";
import { useTasks, useAgents, useCreateTask, useTaskEvents } from "@/lib/queries";
import { Button, Badge, Dialog, Field, inputCls, PageHeader, Empty } from "@/components/ui";
import { cn } from "@/lib/utils";
import type { Task, TaskStatus } from "@/lib/types";

const STATUS_TABS: ("all" | TaskStatus)[] = [
  "all",
  "backlog",
  "queued",
  "running",
  "waiting_children",
  "completed",
  "failed",
  "cancelled",
];

export function TaskList() {
  useTaskEvents();
  const { data: tasks, isLoading } = useTasks();
  const { data: agents } = useAgents();
  const [filter, setFilter] = useState<"all" | TaskStatus>("all");
  const [showForm, setShowForm] = useState(false);

  const agentName = (id: string) => agents?.find((a) => a.id === id)?.name ?? (id || "-");
  const filtered = tasks?.filter((t) => filter === "all" || t.status === filter);

  return (
    <div className="p-8">
      <PageHeader
        title="Task"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {showForm && <NewTaskForm onClose={() => setShowForm(false)} />}

      {/* 状态过滤 */}
      <div className="flex gap-1 mb-4 flex-wrap">
        {STATUS_TABS.map((s) => (
          <button
            key={s}
            onClick={() => setFilter(s)}
            className={cn(
              "px-3 py-1 text-xs font-medium rounded-md transition-colors",
              filter === s
                ? "bg-zinc-900 text-white"
                : "bg-white border border-zinc-200 text-zinc-600 hover:bg-zinc-50"
            )}
          >
            {s === "all" ? "全部" : s}
          </button>
        ))}
      </div>

      {isLoading && <p className="text-sm text-zinc-400">加载中…</p>}

      {filtered && filtered.length > 0 && (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">标题</th>
                <th className="px-4 py-3">分配给</th>
                <th className="px-4 py-3">状态</th>
                <th className="px-4 py-3">创建时间</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((t: Task) => (
                <tr key={t.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3 font-medium">
                    <Link href={`/tasks/${t.id}`} className="text-zinc-900 hover:text-blue-600 hover:underline">
                      {t.title}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-zinc-600">
                    {t.assignee_type === "agent" ? agentName(t.assignee_id) : t.assignee_id || "-"}
                  </td>
                  <td className="px-4 py-3"><Badge status={t.status} /></td>
                  <td className="px-4 py-3 text-zinc-400">{new Date(t.created_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {filtered && filtered.length === 0 && (
        <Empty>没有符合条件的 task。</Empty>
      )}
    </div>
  );
}

function NewTaskForm({ onClose }: { onClose: () => void }) {
  const create = useCreateTask();
  const { data: agents } = useAgents();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [assigneeId, setAssigneeId] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate(
      {
        title,
        description,
        assignee_type: assigneeId ? "agent" : "human",
        assignee_id: assigneeId,
        status: assigneeId ? "queued" : "backlog",
      },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="新建 Task"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="task-form" disabled={create.isPending}>
            {create.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="task-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="标题">
          <input value={title} onChange={(e) => setTitle(e.target.value)} className={inputCls} required />
        </Field>
        <Field label="描述">
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} className={inputCls} rows={4} />
        </Field>
        <Field label="分配给 Agent" hint="不选则进入 backlog">
          <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls}>
            <option value="">不分配（backlog）</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        {create.isError && (
          <p className="text-sm text-red-500">{String(create.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
