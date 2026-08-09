"use client";

import { useState } from "react";
import { useAgents, useSquads, useAssignGoal, useCancelGoal, useWaitGoalChildren, useDeleteGoal, useResolveGoalReview, useGoalRuns } from "@/lib/queries";
import { Button, Dialog, Field, inputCls, ConfirmDialog } from "@/components/ui";
import type { Goal } from "@/lib/types";

export function GoalActions({ goal }: { goal: Goal }) {
  const assign = useAssignGoal();
  const cancel = useCancelGoal();
  const wait = useWaitGoalChildren();
  const deleteGoal = useDeleteGoal();
  const [showAssign, setShowAssign] = useState(false);
  const [showDelete, setShowDelete] = useState(false);

  const isTerminal =
    goal.status === "done" || goal.status === "failed" || goal.status === "cancelled";
  const canWait = goal.status === "active";

  return (
    <div className="flex gap-2 flex-wrap items-center">
      <Button onClick={() => setShowAssign(true)}>分配</Button>

      {!isTerminal && (
        <Button variant="danger" onClick={() => cancel.mutate(goal.id)} disabled={cancel.isPending}>
          {cancel.isPending ? "取消中…" : "取消 Goal"}
        </Button>
      )}

      {canWait && (
        <Button variant="outline" onClick={() => wait.mutate(goal.id)} disabled={wait.isPending}>
          {wait.isPending ? "…" : "等待子 Goal"}
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

      {(assign.isError || cancel.isError || wait.isError || deleteGoal.isError) && (
        <p className="text-sm text-red-500 w-full">
          {String(assign.error ?? cancel.error ?? wait.error ?? deleteGoal.error)}
        </p>
      )}
    </div>
  );
}

// ReviewPanel is the human checkpoint (DESIGN.v2.md §4): the goal is parked
// in review — the human decides approve (platform delivers: merge + re-verify
// + push) or reject (back to the agent with the reason as the next scope).
// The evidence bundle from the last run is shown so the decision is made on
// facts, not trust.
function ReviewPanel({ goal }: { goal: Goal }) {
  const resolve = useResolveGoalReview();
  const { data: runs } = useGoalRuns(goal.id);
  const [reason, setReason] = useState("");
  const [showRejectForm, setShowRejectForm] = useState(false);

  // Latest run's evidence (diff stats + verify output + agent summary).
  const lastRun = runs?.filter((r) => r.status === "completed" || r.status === "failed").at(-1);
  let evidence: Record<string, unknown> | null = null;
  if (lastRun?.evidence) {
    try { evidence = JSON.parse(lastRun.evidence); } catch { /* not JSON */ }
  }

  return (
    <div className="rounded border border-amber-300 bg-amber-50 p-4 space-y-3 w-full">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-xs font-mono bg-amber-200 px-2 py-0.5 rounded">等待审批</span>
        {goal.review_request && <span className="text-sm text-amber-900">{goal.review_request}</span>}
        {goal.human_iterations > 0 && (
          <span className="text-xs text-amber-700">已驳回 {goal.human_iterations} 次</span>
        )}
      </div>

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
      {lastRun?.result_summary && (
        <p className="text-xs text-amber-800">
          <span className="font-medium">Agent 汇报：</span>
          {lastRun.result_summary.slice(0, 400)}
        </p>
      )}

      {!showRejectForm ? (
        <div className="flex gap-2">
          <Button onClick={() => resolve.mutate({ id: goal.id, decision: "approve" })} disabled={resolve.isPending}>
            {resolve.isPending ? "处理中…" : "批准并自动合入"}
          </Button>
          <Button variant="outline" onClick={() => setShowRejectForm(true)}>驳回</Button>
        </div>
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            resolve.mutate(
              { id: goal.id, decision: "reject", reason },
              { onSuccess: () => { setShowRejectForm(false); setReason(""); } }
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
