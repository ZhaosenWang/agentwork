"use client";

import { useParams } from "next/navigation";
import Link from "next/link";
import { useTask, useTasks, useAgents, useTaskEvents } from "@/lib/queries";
import { TaskStream } from "@/components/task-stream";
import { TaskActions } from "@/components/task-actions";
import { Badge, Empty } from "@/components/ui";
import type { Task } from "@/lib/types";

export default function TaskDetailPage() {
  useTaskEvents();
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data: task, isLoading } = useTask(id);
  const { data: allTasks } = useTasks();
  const { data: agents } = useAgents();

  if (isLoading) return <div className="p-8 text-sm text-zinc-400">加载中…</div>;
  if (!task) return <div className="p-8 text-sm text-zinc-400">找不到 task。</div>;

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const children = allTasks?.filter((t) => t.parent_id === id) ?? [];

  return (
    <div className="p-8 space-y-5 max-w-4xl">
      {/* 顶部 */}
      <div>
        <Link href="/tasks" className="text-sm text-zinc-400 hover:text-zinc-700 hover:underline">
          ← 返回列表
        </Link>
        <div className="flex items-center gap-3 mt-3">
          <h1 className="text-lg font-semibold text-zinc-900">{task.title}</h1>
          <Badge status={task.status} />
        </div>
        <p className="text-sm text-zinc-500 mt-1">
          分配给：{task.assignee_type === "agent" ? agentName(task.assignee_id) : (task.assignee_id || "-")}
        </p>
        {task.description && (
          <p className="text-sm text-zinc-600 mt-3 whitespace-pre-wrap">{task.description}</p>
        )}
        {task.handoff_note && (
          <div className="mt-3 p-3 bg-amber-50 border border-amber-200 rounded-lg text-sm text-amber-800 whitespace-pre-wrap">
            <span className="font-medium">Handoff note：</span>
            {task.handoff_note}
          </div>
        )}
      </div>

      {/* 操作 */}
      <TaskActions task={task} />

      {/* 流式输出 */}
      <TaskStream taskId={id} />

      {/* 子任务 */}
      <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
        <div className="px-4 py-2.5 border-b border-zinc-100 text-xs font-medium text-zinc-500 uppercase tracking-wide">
          子任务{children.length > 0 && `（${children.length}）`}
        </div>
        <div className="p-4">
          {children.length === 0 ? (
            <Empty>没有子任务。</Empty>
          ) : (
            <ul className="space-y-2">
              {children.map((c: Task) => (
                <li key={c.id} className="flex items-center gap-2 text-sm">
                  <Link href={`/tasks/${c.id}`} className="font-medium text-zinc-900 hover:text-blue-600 hover:underline">
                    {c.title}
                  </Link>
                  <Badge status={c.status} />
                  <span className="text-zinc-400 text-xs">
                    {c.assignee_type === "agent" ? agentName(c.assignee_id) : c.assignee_id}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </div>
  );
}
