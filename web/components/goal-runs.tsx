"use client";

import { useState, useMemo } from "react";
import { useGoalRuns, useGoalRunMessages, useAgents } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Badge, Empty } from "@/components/ui";
import type { Run } from "@/lib/types";
import type { ChatMessage } from "@/lib/api";

export function GoalRuns({ goalId }: { goalId: string }) {
  const { data: runs, isLoading, refetch } = useGoalRuns(goalId);
  const { data: agents } = useAgents();
  const [showPast, setShowPast] = useState(false);

  // Agent id → name (the runs table reads who is working, not a hex id).
  const agentName = (id: string) => agents?.find((a) => a.id === id)?.name ?? id.slice(0, 8);

  // Refresh on run events
  useWSEvent("run:enqueued", () => refetch());
  useWSEvent("run:event", () => refetch());

  const activeRuns = useMemo(() => runs?.filter((r) => r.status === "queued" || r.status === "running") ?? [], [runs]);
  const pastRuns = useMemo(
    () => (runs?.filter((r) => r.status !== "queued" && r.status !== "running") ?? [])
      .sort((a, b) => {
        const aTime = a.finished_at || a.started_at || a.created_at || "";
        const bTime = b.finished_at || b.started_at || b.created_at || "";
        return bTime.localeCompare(aTime);
      }),
    [runs]
  );

  return (
    <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
      <div className="px-4 py-2.5 border-b border-zinc-100 text-xs font-medium text-zinc-500 uppercase tracking-wide">
        运行历史{runs && runs.length > 0 && `（${runs.length}）`}
      </div>
      <div className="p-4">
        {isLoading ? (
          <div className="text-sm text-zinc-400 text-center py-8">加载中…</div>
        ) : !runs || runs.length === 0 ? (
          <Empty>尚未有运行记录。</Empty>
        ) : (
          <div className="space-y-3">
            {/* Active runs */}
            {activeRuns.length > 0 && (
              <div>
                <div className="text-xs font-medium text-zinc-500 mb-2">活跃运行</div>
                <RunTable runs={activeRuns} goalId={goalId} agentName={agentName} />
              </div>
            )}

            {/* Past runs (collapsible) */}
            {pastRuns.length > 0 && (
              <div>
                <button
                  onClick={() => setShowPast(!showPast)}
                  className="text-xs font-medium text-zinc-500 hover:text-zinc-700 mb-2 flex items-center gap-1"
                >
                  {showPast ? "▾" : "▸"} 历史运行（{pastRuns.length}）
                </button>
                {showPast && <RunTable runs={pastRuns} goalId={goalId} agentName={agentName} />}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function RunTable({ runs, goalId, agentName }: { runs: Run[]; goalId: string; agentName: (id: string) => string }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-zinc-100">
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">Agent</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">状态</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">尝试</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">结果</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">开始</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">结束</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500"></th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <RunRow key={r.id} run={r} goalId={goalId} agentName={agentName} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

// RunRow shows one run; clicking the chevron opens the LIVE interaction
// stream (chat_message — what the agent is doing right now), refreshed on
// every run:event over the WS.
function RunRow({ run, goalId, agentName }: { run: Run; goalId: string; agentName: (id: string) => string }) {
  const [open, setOpen] = useState(false);
  const { data: messages, refetch } = useGoalRunMessages(goalId, run.id);
  useWSEvent("run:event", () => {
    if (open) refetch();
  });

  return (
    <>
      <tr className="border-b border-zinc-50 hover:bg-zinc-50/50">
        <td className="py-1.5 px-2 text-zinc-700 font-medium text-xs">{run.agent_id ? agentName(run.agent_id) : "-"}</td>
        <td className="py-1.5 px-2"><Badge status={run.status} /></td>
        <td className="py-1.5 px-2 text-zinc-500">#{run.attempt}</td>
        <td className="py-1.5 px-2 text-zinc-500 max-w-[200px] truncate">{run.result_summary || "-"}</td>
        <td className="py-1.5 px-2 text-zinc-400">{run.started_at ? new Date(run.started_at).toLocaleString("zh-CN") : "-"}</td>
        <td className="py-1.5 px-2 text-zinc-400">{run.finished_at ? new Date(run.finished_at).toLocaleString("zh-CN") : "-"}</td>
        <td className="py-1.5 px-2">
          <button onClick={() => setOpen(!open)} className="text-zinc-400 hover:text-zinc-600 text-xs">
            {open ? "▾" : "▸"}
          </button>
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={7} className="py-2 px-3 bg-zinc-50/60">
            {run.status === "running" && (
              <div className="text-[11px] text-emerald-600 mb-1.5 flex items-center gap-1">
                <span className="inline-block w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse" />
                运行中——实时交互流
              </div>
            )}
            <RunMessages messages={messages ?? []} />
          </td>
        </tr>
      )}
    </>
  );
}

// RunMessages renders the interaction stream: assistant text, thoughts, and
// tool calls (each tool use shows its name + input).
function RunMessages({ messages }: { messages: ChatMessage[] }) {
  if (messages.length === 0) {
    return <div className="text-[11px] text-zinc-400 py-2">尚无交互记录…</div>;
  }
  return (
    <div className="max-h-72 overflow-y-auto space-y-1.5">
      {messages.map((m, i) => {
        if (m.role === "tool" && m.tool_calls) {
          try {
            const tc = JSON.parse(m.tool_calls);
            if (tc.type === "tool_use") {
              const input = typeof tc.input === "string" ? tc.input : JSON.stringify(tc.input ?? "");
              return (
                <div key={i} className="text-[11px]">
                  <span className="text-purple-600 font-medium">⚙ {tc.tool}</span>
                  <span className="text-zinc-400 ml-1 break-all">{String(input).slice(0, 160)}</span>
                </div>
              );
            }
            if (tc.type === "tool_result") {
              const out = typeof tc.output === "string" ? tc.output : JSON.stringify(tc.output ?? "");
              return (
                <div key={i} className="text-[11px] pl-3 border-l-2 border-zinc-200">
                  <span className="text-zinc-400 break-all whitespace-pre-wrap">{String(out).slice(0, 240)}</span>
                </div>
              );
            }
          } catch {
            /* not tool JSON */
          }
        }
        if (m.role === "thought") {
          return (
            <div key={i} className="text-[11px] text-zinc-400 italic">
              💭 {m.content}
            </div>
          );
        }
        return (
          <div key={i} className="text-[11px] text-zinc-700">
            {m.content}
          </div>
        );
      })}
    </div>
  );
}
