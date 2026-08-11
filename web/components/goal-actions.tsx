"use client";

import { useState } from "react";
import { useAgents, useSquads, useAssignGoal, useCancelGoal,
  useReopenGoal, useDeleteGoal, useResolveGoalReview, useGoalRuns, useGoalComments } from "@/lib/queries";
import { Button, Dialog, Field, inputCls, ConfirmDialog } from "@/components/ui";
import { Markdown } from "@/components/markdown";
import type { Goal } from "@/lib/types";

export function GoalActions({ goal }: { goal: Goal }) {
  const assign = useAssignGoal();
  const cancel = useCancelGoal();
  const deleteGoal = useDeleteGoal();
  const reopen = useReopenGoal();
  const [showAssign, setShowAssign] = useState(false);
  const [showDelete, setShowDelete] = useState(false);

  const isTerminal =
    goal.status === "done" || goal.status === "failed" || goal.status === "cancelled";

  return (
    <div className="flex gap-2 flex-wrap items-center">
      <Button onClick={() => setShowAssign(true)}>分配</Button>

      {!isTerminal && (
        <Button variant="danger" onClick={() => cancel.mutate(goal.id)} disabled={cancel.isPending}>
          {cancel.isPending ? "取消中…" : "取消 Goal"}
        </Button>
      )}

      {(goal.status === "failed" || goal.status === "cancelled") && (
        <Button variant="outline" onClick={() => reopen.mutate(goal.id)} disabled={reopen.isPending}>
          {reopen.isPending ? "重开中…" : "重开（失败/取消后人工接手）"}
        </Button>
      )}

      <Button variant="ghost" onClick={() => setShowDelete(true)}>
        删除
      </Button>

      {goal.status === "review" && <ReviewPanel goal={goal} />}

      {showAssign && <AssignDialog goal={goal} onClose={() => setShowAssign(false)} />}
      {showDelete && (
        <ConfirmDialog
          title="确认删除"
          message={`确定要删除 Goal「${goal.title}」吗？此操作不可撤销。`}
          onConfirm={() => deleteGoal.mutate(goal.id, { onSuccess: () => setShowDelete(false) })}
          onClose={() => setShowDelete(false)}
          loading={deleteGoal.isPending}
        />
      )}

      {(assign.isError || cancel.isError || deleteGoal.isError) && (
        <p className="text-sm text-red-500 w-full">
          {String(assign.error ?? cancel.error ?? deleteGoal.error)}
        </p>
      )}
    </div>
  );
}

// ReviewPanel is the human checkpoint (DESIGN.md §4): the goal is parked
// in review — the human decides approve (platform delivers: merge + re-verify
// + push) or reject (back to the agent with the reason as the next scope).
// The evidence bundle from the last run is shown so the decision is made on
// facts, not trust.
function ReviewPanel({ goal }: { goal: Goal }) {
  const resolve = useResolveGoalReview();
  const { data: runs } = useGoalRuns(goal.id);
  const { data: comments } = useGoalComments(goal.id);
  const { data: agents } = useAgents();
  const [reason, setReason] = useState("");
  const [showRejectForm, setShowRejectForm] = useState(false);
  const [justResolved, setJustResolved] = useState<string | null>(null);

  const agentName = (id: string) => agents?.find((a) => a.id === id)?.name ?? id.slice(0, 8);
  // 审查意见 / 人的话 — 审批时直接可见，不用滚到评论区（squad 审查的
  // 价值就在审批这一刻）。最近 4 条，倒序。
  const recentComments = (comments ?? []).slice(-4).reverse();

  // The deliver step runs ASYNC after approve — the goal stays in review
  // until merge+re-verify+push finishes (or fails back). Give the human
  // immediate feedback instead of a dead button (the regression: approving
  // showed no change and got clicked 4 times).
  const resolveOutcome =
    justResolved ?? (goal.review_request?.startsWith("deliver:") ? "deliver_failed" : null);

  // Latest run's evidence (diff stats + verify output + agent summary).
  const lastRun = runs?.filter((r) => r.status === "completed" || r.status === "failed").at(-1);
  let evidence: Record<string, unknown> | null = null;
  if (lastRun?.evidence) {
    try { evidence = JSON.parse(lastRun.evidence); } catch { /* not JSON */ }
  }

  const agentComments = recentComments.filter((c) => c.author_type === "agent");
  const otherComments = recentComments.filter((c) => c.author_type !== "agent");

  return (
    <div className="rounded-2xl border border-amber-300/80 bg-amber-50/70 w-full shadow-sm shadow-amber-500/5 overflow-hidden">
      {/* 卡点原因（独立行，小字两行排） */}
      <div className="px-4 py-3 border-b border-amber-200/60 space-y-1.5">
        <div className="flex items-center gap-2">
          <span className="text-xs font-mono bg-amber-200 px-2 py-0.5 rounded">等待审批</span>
          {goal.human_iterations > 0 && (
            <span className="text-xs text-amber-700">已驳回 {goal.human_iterations} 次</span>
          )}
        </div>
        {goal.review_request && (
          <p className="text-xs text-amber-900/80 leading-relaxed">{goal.review_request}</p>
        )}
      </div>

      <div className="p-4 space-y-3">
        {/* 审查意见：最新 agent 评论置顶（白底卡片与琥珀底区分——审批先看它） */}
        {agentComments.length > 0 && (
          <div className="bg-white rounded-xl border border-zinc-200/80 p-3 space-y-2">
            <div className="text-[11px] font-medium text-zinc-500 uppercase tracking-wide">审查意见</div>
            {agentComments.map((c) => (
              <div key={c.id}>
                <span className="font-medium text-zinc-800 text-xs">{agentName(c.author_id)}</span>
                <div className="max-h-48 overflow-y-auto mt-1">
                  <Markdown content={c.content} agentName={agentName} className="text-zinc-700" />
                </div>
              </div>
            ))}
          </div>
        )}

        {/* Agent 汇报（折叠，默认收起——长文不占审批视线） */}
        {lastRun?.result_summary && (
          <details className="text-xs" open={agentComments.length === 0}>
            <summary className="cursor-pointer font-medium text-amber-900">Agent 汇报</summary>
            <div className="mt-1.5 max-h-56 overflow-y-auto">
              <Markdown content={lastRun.result_summary.slice(0, 500)} agentName={agentName} className="text-amber-800" />
            </div>
          </details>
        )}

        {/* 证据包（折叠） */}
        {evidence && (
          <details className="text-xs">
            <summary className="cursor-pointer text-amber-800">证据包（{lastRun?.id.slice(0, 8)}）</summary>
            <pre className="mt-2 whitespace-pre-wrap bg-amber-100 p-2 rounded text-amber-900 max-h-64 overflow-auto">
              {String(evidence.diff_stat ?? "")}
              {"\n"}
              {String(evidence.verify ?? "")}
            </pre>
          </details>
        )}

        {/* 其他交流（人的话 / 系统消息，折叠） */}
        {otherComments.length > 0 && (
          <details className="text-xs">
            <summary className="cursor-pointer font-medium text-amber-900">其他交流（{otherComments.length}）</summary>
            <div className="mt-1.5 space-y-1.5">
              {otherComments.map((c) => {
                const author =
                  c.author_type === "human" ? "你" : "系统";
                return (
                  <div key={c.id} className="bg-amber-100/60 rounded-lg p-2">
                    <span className="font-medium text-amber-900">{author}</span>
                    <Markdown content={c.content} agentName={agentName} className="text-amber-800 mt-0.5" />
                  </div>
                );
              })}
            </div>
          </details>
        )}

      {resolveOutcome === "approved" && (
        <p className="text-sm text-emerald-700 font-medium">✅ 已批准——平台正在合入（merge + 复验 + push），完成后此卡自动关闭。</p>
      )}
      {resolveOutcome === "rejected" && (
        <p className="text-sm text-amber-800 font-medium">↩️ 已驳回——agent 将带你的理由重新执行。</p>
      )}
      {resolveOutcome === "deliver_failed" && (
        <p className="text-sm text-red-700 font-medium">⚠️ 上次合入失败（冲突/验证红）——可重新批准重试合入，或驳回让 agent 修。</p>
      )}

      {!showRejectForm ? (
        <div className="flex gap-2">
          <Button
            onClick={() =>
              resolve.mutate(
                { id: goal.id, decision: "approve" },
                { onSuccess: () => setJustResolved("approved") }
              )
            }
            disabled={resolve.isPending || resolveOutcome === "approved"}
          >
            {resolve.isPending ? "处理中…" : resolveOutcome === "approved" ? "已批准，合入中…" : "批准并自动合入"}
          </Button>
          <Button variant="outline" onClick={() => setShowRejectForm(true)} disabled={resolveOutcome === "approved"}>
            驳回
          </Button>
        </div>
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            resolve.mutate(
              { id: goal.id, decision: "reject", reason },
              { onSuccess: () => { setShowRejectForm(false); setReason(""); setJustResolved("rejected"); } }
            );
          }}
          className="space-y-2"
        >
          <textarea
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            className={inputCls}
            rows={3}
            placeholder="驳回理由——会成为 agent 下一轮的执行范围"
          />
          <div className="flex gap-2">
            <Button type="submit" variant="danger" disabled={resolve.isPending || !reason.trim()}>
              {resolve.isPending ? "退回中…" : "驳回并退回 agent"}
            </Button>
            <Button variant="ghost" onClick={() => setShowRejectForm(false)}>返回</Button>
          </div>
        </form>
      )}
      {resolve.isError && <p className="text-sm text-red-500">{String(resolve.error)}</p>}
      </div>
    </div>
  );
}

function AssignDialog({ goal, onClose }: { goal: Goal; onClose: () => void }) {
  const assign = useAssignGoal();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const [assigneeType, setAssigneeType] = useState(goal.assignee_type || "agent");
  const [assigneeId, setAssigneeId] = useState(goal.assignee_id || "");
  const [handoff, setHandoff] = useState(goal.handoff_note || "");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    assign.mutate(
      { id: goal.id, assignee_type: assigneeType, assignee_id: assigneeId, handoff_note: handoff },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="分配 Goal"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="assign-form" disabled={assign.isPending}>
            {assign.isPending ? "分配中…" : "分配"}
          </Button>
        </>
      }
    >
      <form id="assign-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="负责人类型">
          <select value={assigneeType} onChange={(e) => { setAssigneeType(e.target.value); setAssigneeId(""); }} className={inputCls}>
            <option value="agent">Agent</option>
            <option value="squad">Squad</option>
          </select>
        </Field>
        {assigneeType === "agent" && (
          <Field label="选择 Agent">
            <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {agents?.map((a) => (
                <option key={a.id} value={a.id}>{a.name}</option>
              ))}
            </select>
          </Field>
        )}
        {assigneeType === "squad" && (
          <Field label="选择 Squad">
            <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {squads?.map((s) => (
                <option key={s.id} value={s.id}>{s.name}</option>
              ))}
            </select>
          </Field>
        )}
        <Field label="Handoff Note" hint="告诉 agent 这次运行的范围/约束">
          <textarea value={handoff} onChange={(e) => setHandoff(e.target.value)} className={inputCls} rows={4} placeholder="告诉 agent 这次运行的范围/约束…" />
        </Field>
        {assign.isError && (
          <p className="text-sm text-red-500">{String(assign.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
