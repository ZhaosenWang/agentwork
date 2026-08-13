"use client";

import { useState } from "react";
import { useGoalChanges, useSubGoals } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Empty } from "@/components/ui";
import type { ChangeDetail } from "@/lib/types";

const CHANGE_STATUS_COLORS: Record<string, string> = {
  ready: "bg-blue-50 text-blue-700 dot-blue",
  integrating: "bg-indigo-50 text-indigo-700 dot-indigo",
  integrated: "bg-emerald-50 text-emerald-700 dot-emerald",
  conflict: "bg-red-50 text-red-700 dot-red",
};

const CHANGE_STATUS_LABEL: Record<string, string> = {
  ready: "待集成",
  integrating: "集成中",
  integrated: "已集成",
  conflict: "冲突",
};

const short = (ref: string) => (ref ? ref.slice(0, 8) : "");

// ChangeStatus renders a change's 4-state badge.
function ChangeStatus({ status }: { status: string }) {
  const colors = CHANGE_STATUS_COLORS[status] ?? "bg-zinc-100 text-zinc-600 dot-zinc";
  return (
    <span
      className={`inline-flex items-center gap-1.5 px-2 py-0.5 text-[11px] font-medium rounded-full ${colors.replace(/ dot-\S+/, "")}`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${colors.split(" ").find((c) => c.startsWith("dot-")) ?? "bg-zinc-400"}`} />
      {CHANGE_STATUS_LABEL[status] ?? status}
    </span>
  );
}

// GoalChanges renders the goal's change panel (v2 model): the logical
// deliverables sub-goals produced, with their revision history — the owner's
// integration view. Refreshes on change.* / sub_goal.* WS events.
export function GoalChanges({ goalId }: { goalId: string }) {
  const { data: changes, refetch } = useGoalChanges(goalId);
  const { data: subGoals } = useSubGoals(goalId);
  const [expandedId, setExpandedId] = useState("");

  useWSEvent("change.ready", () => refetch());
  useWSEvent("change.integrated", () => refetch());
  useWSEvent("change.conflict", () => refetch());
  useWSEvent("sub_goal.verified", () => refetch());

  const subGoalTitle = (subGoalId: string) =>
    subGoals?.find((sg) => sg.id === subGoalId)?.title ?? subGoalId.slice(0, 8);

  return (
    <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
      <div className="px-4 py-2.5 border-b border-zinc-100">
        <span className="text-xs font-medium text-zinc-500 uppercase tracking-wide">
          变更{changes && changes.length > 0 && `（${changes.length}）`}
        </span>
      </div>
      <div className="p-4">
        {!changes || changes.length === 0 ? (
          <Empty icon="🧩">还没有变更。子任务验证通过后会产出一个 Change，Owner 在这里看到待集成的交付物。</Empty>
        ) : (
          <ul className="space-y-2">
            {changes.map((c: ChangeDetail) => {
              const expanded = expandedId === c.id;
              return (
                <li key={c.id} className="text-sm border border-zinc-100 rounded-lg">
                  <button
                    onClick={() => setExpandedId(expanded ? "" : c.id)}
                    className="w-full flex items-center gap-2 px-3 py-2 text-left hover:bg-zinc-50/60 rounded-lg transition-colors"
                  >
                    <span className={`transition-transform text-zinc-400 ${expanded ? "rotate-90" : ""}`}>▸</span>
                    <span className="font-medium text-zinc-900 truncate max-w-[40%]">{subGoalTitle(c.sub_goal_id)}</span>
                    <ChangeStatus status={c.status} />
                    <span className="text-zinc-400 text-xs font-mono ml-auto">head {short(c.head_ref)}</span>
                    <span className="text-zinc-400 text-xs">
                      {c.revisions.length > 0 && `rev ${c.revisions[c.revisions.length - 1].seq}`}
                    </span>
                  </button>
                  {expanded && (
                    <div className="px-3 pb-3 pt-1 space-y-1 border-t border-zinc-100">
                      <p className="text-[11px] text-zinc-400 font-mono">change {c.id}</p>
                      {c.revisions.map((r) => (
                        <div key={r.id} className="flex items-center gap-2 text-xs text-zinc-500">
                          <span className="font-mono text-zinc-400">#{r.seq}</span>
                          <span>
                            基线 <code className="font-mono">{short(r.base_ref)}</code>
                            {" → "}交付 <code className="font-mono">{short(r.head_ref)}</code>
                          </span>
                          <span className="text-zinc-300">
                            {new Date(r.created_at).toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" })}
                          </span>
                        </div>
                      ))}
                      {c.status === "conflict" && (
                        <p className="text-xs text-red-600 pt-1">
                          集成时冲突——子任务 assignee 已被唤醒返修，新修订会追加到这个 Change 上。
                        </p>
                      )}
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}
