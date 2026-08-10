"use client";

import Link from "next/link";
import { ArrowRight } from "lucide-react";
import { useGoals, useGoalEvents } from "@/lib/queries";
import { GoalStats } from "@/components/goal-stats";
import { PageHeader } from "@/components/ui";

export default function Home() {
  useGoalEvents();
  const { data: goals, isLoading } = useGoals();

  return (
    <div className="p-8">
      <PageHeader
        title="概览"
        action={
          <Link
            href="/goals"
            className="inline-flex items-center gap-1.5 px-3 py-1.5 text-sm font-medium rounded-md transition-colors bg-zinc-900 text-white hover:bg-zinc-700"
          >
            查看全部 Goal
            <ArrowRight className="h-4 w-4" />
          </Link>
        }
      />

      {/* Stats cards — data from GET /goals via useGoals */}
      <GoalStats goals={goals} loading={isLoading} />

      <p className="text-sm text-zinc-400">
        状态统计基于当前全部 Goal。如需查看与管理完整任务列表，请前往{" "}
        <Link href="/goals" className="text-blue-600 hover:underline">
          Goal 列表页
        </Link>
        。
      </p>
    </div>
  );
}
