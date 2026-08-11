"use client";

import Link from "next/link";
import { useGoals, useAgents, useSquads, useGoalEvents } from "@/lib/queries";
import { Badge, PageHeader, Button, Empty } from "@/components/ui";
import type { Goal } from "@/lib/types";

// 总览：开门三件事——有什么要我批（待审批队列）、什么在跑（活跃任务）、
// 最近完成了什么（最近完成）。WS 订阅保证卡点变化实时进队列。
export default function Home() {
  useGoalEvents();
  const { data: goals } = useGoals();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;
  const assigneeLabel = (g: Goal) =>
    g.assignee_type === "squad" ? squadName(g.assignee_id) : g.assignee_id ? agentName(g.assignee_id) : "-";

  const all = goals ?? [];
  const review = all.filter((g) => g.status === "review");
  const active = all.filter((g) => g.status === "active");
  const done = all.filter((g) => g.status === "done").slice(0, 5);
  const failed = all.filter((g) => g.status === "failed");

  return (
    <div className="p-8 space-y-7 max-w-5xl page-enter">
      <PageHeader
        title="总览"
        action={
          <Link href="/goals">
            <Button variant="outline">全部任务 →</Button>
          </Link>
        }
      />

      {/* 统计行 */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        <StatCard label="待审批" value={review.length} icon="🔔" gradient="from-purple-500 to-fuchsia-500" />
        <StatCard label="运行中" value={active.length} icon="⚡" gradient="from-blue-500 to-indigo-500" />
        <StatCard label="已完成" value={all.filter((g) => g.status === "done").length} icon="✅" gradient="from-emerald-500 to-teal-500" />
        <StatCard label="失败" value={failed.length} icon="⚠️" gradient={failed.length > 0 ? "from-red-500 to-rose-500" : "from-zinc-400 to-zinc-500"} />
      </div>

      {/* 待审批队列 */}
      <section className="space-y-3">
        <SectionTitle>
          待审批
          {review.length > 0 && <span className="ml-1.5 text-purple-600 font-semibold">{review.length}</span>}
        </SectionTitle>
        {review.length === 0 ? (
          <Empty>没有待审批的卡点——都干净。</Empty>
        ) : (
          <div className="space-y-2">
            {review.map((g) => (
              <Link
                key={g.id}
                href={`/goals/${g.id}`}
                className="block bg-white rounded-2xl border border-purple-200/70 shadow-sm shadow-purple-500/5 hover:border-purple-300 hover:shadow-md hover:shadow-purple-500/10 hover:-translate-y-0.5 transition-all duration-200 p-4 pl-5 border-l-4 border-l-purple-400"
              >
                <div className="flex items-center gap-2 flex-wrap">
                  <span className="font-medium text-zinc-900">{g.title}</span>
                  <Badge status={g.status} />
                  <span className="text-xs text-zinc-400">负责：{assigneeLabel(g)}</span>
                  <span className="ml-auto text-xs text-purple-500 font-semibold">去审批 →</span>
                </div>
                {g.review_request && (
                  <p className="mt-1.5 text-sm text-zinc-600 line-clamp-2">{g.review_request}</p>
                )}
              </Link>
            ))}
          </div>
        )}
      </section>

      {/* 活跃任务 */}
      <section className="space-y-3">
        <SectionTitle>运行中（{active.length}）</SectionTitle>
        {active.length === 0 ? (
          <Empty>没有正在运行的任务。</Empty>
        ) : (
          <div className="bg-white rounded-2xl border border-zinc-200/80 shadow-sm divide-y divide-zinc-50 overflow-hidden">
            {active.map((g) => (
              <Link key={g.id} href={`/goals/${g.id}`} className="flex items-center gap-3 p-3.5 hover:bg-indigo-50/40 transition-colors group">
                <span className="h-2 w-2 rounded-full bg-green-500 animate-pulse shrink-0" />
                <span className="font-medium text-sm text-zinc-900 group-hover:text-indigo-700 transition-colors">{g.title}</span>
                {g.current_agent_id ? (
                  <span className="text-xs font-medium text-emerald-600">{agentName(g.current_agent_id)} 正在执行</span>
                ) : (
                  <span className="text-xs text-zinc-400">{assigneeLabel(g)}</span>
                )}
                <span className="text-xs text-zinc-400 ml-auto">
                  {g.created_at ? new Date(g.created_at).toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : ""}
                </span>
              </Link>
            ))}
          </div>
        )}
      </section>

      {/* 最近完成 */}
      {done.length > 0 && (
        <section className="space-y-3">
          <SectionTitle>最近完成</SectionTitle>
          <div className="bg-white rounded-2xl border border-zinc-200/80 shadow-sm divide-y divide-zinc-50 overflow-hidden">
            {done.map((g) => (
              <Link key={g.id} href={`/goals/${g.id}`} className="flex items-center gap-3 p-3.5 hover:bg-emerald-50/40 transition-colors group">
                <Badge status="done" />
                <span className="font-medium text-sm text-zinc-900 group-hover:text-emerald-700 transition-colors">{g.title}</span>
                <span className="text-xs text-zinc-400 ml-auto">
                  {g.created_at ? new Date(g.created_at).toLocaleString("zh-CN") : ""}
                </span>
              </Link>
            ))}
          </div>
        </section>
      )}
    </div>
  );
}

function StatCard({ label, value, icon, gradient }: { label: string; value: number; icon: string; gradient: string }) {
  return (
    <div
      className={`bg-gradient-to-br ${gradient} text-white rounded-2xl p-4 shadow-md shadow-black/5 hover:shadow-lg hover:-translate-y-0.5 transition-all duration-200`}
    >
      <div className="flex items-center justify-between">
        <div className="text-3xl font-semibold">{value}</div>
        <span className="text-xl opacity-80">{icon}</span>
      </div>
      <div className="text-xs mt-1 opacity-85 font-medium">{label}</div>
    </div>
  );
}

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h2 className="text-xs font-medium text-zinc-500 uppercase tracking-wide">{children}</h2>;
}
