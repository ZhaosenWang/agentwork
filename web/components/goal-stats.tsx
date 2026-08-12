"use client";

import type { Goal } from "@/lib/types";
import { cn } from "@/lib/utils";
import { Skeleton } from "@/components/ui";

interface StatItem {
  key: "total" | "active" | "pending" | "done";
  label: string;
  hint?: string;
  dot: string; // accent color classes
  count: number;
}

function buildStats(goals: Goal[]): StatItem[] {
  const count = (status: Goal["status"]) =>
    goals.filter((g) => g.status === status).length;
  return [
    {
      key: "total",
      label: "总任务数",
      hint: "全部 Goal",
      dot: "bg-zinc-400",
      count: goals.length,
    },
    {
      key: "active",
      label: "进行中",
      hint: "active",
      dot: "bg-blue-500",
      count: count("active"),
    },
    {
      key: "pending",
      label: "待审批",
      hint: "review",
      dot: "bg-amber-500",
      count: count("review"),
    },
    {
      key: "done",
      label: "已完成",
      hint: "done",
      dot: "bg-emerald-500",
      count: count("done"),
    },
  ];
}

export function GoalStats({
  goals,
  loading,
}: {
  goals?: Goal[];
  loading?: boolean;
}) {
  if (loading || !goals) {
    return (
      <div className="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6">
        {Array.from({ length: 4 }).map((_, i) => (
          <div
            key={i}
            className="bg-white rounded-xl border border-zinc-200 p-4"
          >
            <Skeleton className="h-3 w-16 mb-2" />
            <Skeleton className="h-8 w-12" />
          </div>
        ))}
      </div>
    );
  }

  const stats = buildStats(goals);

  return (
    <div className="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6">
      {stats.map((s) => (
        <div
          key={s.key}
          className="bg-white rounded-xl border border-zinc-200 p-4 flex items-center gap-3"
        >
          <span className={cn("w-2.5 h-2.5 rounded-full shrink-0", s.dot)} />
          <div className="min-w-0">
            <p className="text-xs text-zinc-500 font-medium">
              {s.label}
              {s.hint && (
                <span className="ml-1.5 text-zinc-300 text-[10px]">{s.hint}</span>
              )}
            </p>
            <p className="text-2xl font-semibold text-zinc-900 leading-tight">
              {s.count}
            </p>
          </div>
        </div>
      ))}
    </div>
  );
}
