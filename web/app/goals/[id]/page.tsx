"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { useGoal, useAgents, useSquads, useApproveGoal, useRejectGoal, useGoalEvents } from "@/lib/queries";
import { GoalActions } from "@/components/goal-actions";
import { GoalComments } from "@/components/goal-comments";
import { GoalRuns } from "@/components/goal-runs";
import { Badge, Button, Dialog, Field, inputCls } from "@/components/ui";

export default function GoalDetailPage() {
  useGoalEvents();
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data: goal, isLoading } = useGoal(id);
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const approve = useApproveGoal();
  const reject = useRejectGoal();
  const [showReject, setShowReject] = useState(false);
  const [rejectReason, setRejectReason] = useState("");

  if (isLoading) return <div className="p-8 text-sm text-zinc-400">加载中…</div>;
  if (!goal) return <div className="p-8 text-sm text-zinc-400">找不到 Goal。</div>;

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;
  const assigneeLabel = goal.assignee_type === "squad"
    ? (squadName(goal.assignee_id) || goal.assignee_id || "-")
    : goal.assignee_id
      ? (agentName(goal.assignee_id) || goal.assignee_id)
      : "-";

  return (
    <div className="p-8 space-y-5 max-w-4xl">
      {/* Breadcrumb */}
      <div>
        <Link href="/goals" className="text-sm text-zinc-400 hover:text-zinc-700 hover:underline">
          ← 返回列表
        </Link>
        <div className="flex items-center gap-3 mt-3">
          <h1 className="text-lg font-semibold text-zinc-900">{goal.title}</h1>
          <Badge status={goal.status} />
        </div>
        <p className="text-sm text-zinc-500 mt-1">
          分配给：{assigneeLabel}
        </p>
        {goal.description && (
          <p className="text-sm text-zinc-600 mt-3 whitespace-pre-wrap">{goal.description}</p>
        )}
        {goal.handoff_note && (
          <div className="mt-3 p-3 bg-amber-50 border border-amber-200 rounded-lg text-sm text-amber-800 whitespace-pre-wrap">
            <span className="font-medium">Handoff note：</span>
            {goal.handoff_note}
          </div>
        )}
      </div>

      {/* Human review actions (only for review status) */}
      {goal.status === "review" && (
        <div className="bg-amber-50 border border-amber-200 rounded-xl p-5">
          <h3 className="text-sm font-medium text-amber-800 mb-3">人工审核</h3>
          <p className="text-sm text-amber-700 mb-4">
            该任务已提交审核，请检查运行结果后通过或驳回。
          </p>
          <div className="flex gap-3">
            <Button
              onClick={() =>
                approve.mutate({ id: goal.id, summary: "Approved by human review." })
              }
              disabled={approve.isPending}
            >
              {approve.isPending ? "…" : "通过"}
            </Button>
            <Button
              variant="outline"
              onClick={() => setShowReject(true)}
            >
              驳回
            </Button>
          </div>
          {approve.isError && (
            <p className="text-sm text-red-500 mt-2">{String(approve.error)}</p>
          )}
        </div>
      )}

      {/* Actions */}
      <GoalActions goal={goal} />

      {/* Comments */}
      <GoalComments goalId={id} />

      {/* Runs */}
      <GoalRuns goalId={id} />

      {/* Reject dialog */}
      {showReject && (
        <Dialog
          title="驳回任务"
          onClose={() => setShowReject(false)}
          footer={
            <>
              <Button variant="outline" onClick={() => setShowReject(false)}>取消</Button>
              <Button
                onClick={() => {
                  reject.mutate(
                    { id: goal.id, reason: rejectReason || "未说明原因" },
                    { onSuccess: () => { setShowReject(false); setRejectReason(""); } }
                  );
                }}
                disabled={reject.isPending}
              >
                {reject.isPending ? "…" : "确认驳回"}
              </Button>
            </>
          }
        >
          <Field label="驳回原因" hint="说明为什么需要返工">
            <textarea
              value={rejectReason}
              onChange={(e) => setRejectReason(e.target.value)}
              className={inputCls}
              rows={3}
              placeholder="例如：缺少单元测试、代码未通过验收标准…"
            />
          </Field>
          {reject.isError && (
            <p className="text-sm text-red-500">{String(reject.error)}</p>
          )}
        </Dialog>
      )}
    </div>
  );
}
