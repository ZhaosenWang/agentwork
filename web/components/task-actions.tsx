"use client";

import { useState } from "react";
import { useAgents, useAssignTask, useCancelTask, useWaitChildren } from "@/lib/queries";
import { Button, Dialog, Field, inputCls } from "@/components/ui";
import type { Task } from "@/lib/types";

export function TaskActions({ task }: { task: Task }) {
  const assign = useAssignTask();
  const cancel = useCancelTask();
  const wait = useWaitChildren();
  const [showAssign, setShowAssign] = useState(false);

  const isTerminal =
    task.status === "completed" || task.status === "failed" || task.status === "cancelled";
  const canWait = task.status === "running" || task.status === "queued";

  return (
    <div className="flex gap-2 flex-wrap items-center">
      <Button onClick={() => setShowAssign(true)}>分配</Button>

      {!isTerminal && (
        <Button variant="danger" onClick={() => cancel.mutate(task.id)} disabled={cancel.isPending}>
          {cancel.isPending ? "取消中…" : "取消任务"}
        </Button>
      )}

      {canWait && (
        <Button variant="outline" onClick={() => wait.mutate(task.id)} disabled={wait.isPending}>
          {wait.isPending ? "…" : "等待子任务"}
        </Button>
      )}

      {showAssign && <AssignDialog task={task} onClose={() => setShowAssign(false)} />}

      {(assign.isError || cancel.isError || wait.isError) && (
        <p className="text-sm text-red-500 w-full">
          {String(assign.error ?? cancel.error ?? wait.error)}
        </p>
      )}
    </div>
  );
}

function AssignDialog({ task, onClose }: { task: Task; onClose: () => void }) {
  const assign = useAssignTask();
  const { data: agents } = useAgents();
  const [agentId, setAgentId] = useState(task.assignee_id || "");
  const [handoff, setHandoff] = useState(task.handoff_note || "");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    assign.mutate(
      { id: task.id, assignee_type: "agent", assignee_id: agentId, handoff_note: handoff },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="分配 Task"
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
        <Field label="Agent">
          <select value={agentId} onChange={(e) => setAgentId(e.target.value)} className={inputCls} required>
            <option value="">选择…</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        <Field label="Handoff Note" hint="作用域指令，告诉 agent 这次运行的范围/约束">
          <textarea value={handoff} onChange={(e) => setHandoff(e.target.value)} className={inputCls} rows={4} placeholder="告诉 agent 这次运行的范围/约束…" />
        </Field>
        {assign.isError && (
          <p className="text-sm text-red-500">{String(assign.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
