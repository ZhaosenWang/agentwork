"use client";

import { useState } from "react";
import Link from "next/link";
import { useSquads, useAgents, useCreateSquad, useAddSquadMember, useDeleteSquad, useGoalEvents } from "@/lib/queries";
import { Button, PageHeader, Empty, Dialog, Field, inputCls, ConfirmDialog } from "@/components/ui";
import type { Squad } from "@/lib/types";
import { displayName } from "@/lib/utils";

export default function SquadsPage() {
  useGoalEvents();
  const { data: squads, isLoading } = useSquads();
  const { data: agents } = useAgents(true);
  const createSquad = useCreateSquad();
  const deleteSquad = useDeleteSquad();
  const [showForm, setShowForm] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const agentName = (aid: string) => {
    const a = agents?.find((x) => x.id === aid);
    return a ? displayName(a.name, a.archived_at) : "已删除";
  };

  return (
    <div className="p-8">
      <PageHeader
        title="Squad"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {isLoading ? (
        <div className="text-sm text-zinc-400 py-16 text-center">加载中…</div>
      ) : !squads || squads.length === 0 ? (
        <Empty>暂无 Squad。点「+ 新建」创建一个。</Empty>
      ) : (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">名称</th>
                <th className="px-4 py-3">Leader</th>
                <th className="px-4 py-3">描述</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-20"></th>
              </tr>
            </thead>
            <tbody>
              {squads.map((s: Squad) => (
                <tr key={s.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3 font-medium text-zinc-900">
                    <Link href={`/squads/${s.id}`} className="hover:text-blue-600 hover:underline">
                      {s.name}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-zinc-600">{agentName(s.leader_id)}</td>
                  <td className="px-4 py-3 text-zinc-500 max-w-[200px] truncate">{s.description || "-"}</td>
                  <td className="px-4 py-3 text-zinc-400">{new Date(s.created_at).toLocaleString()}</td>
                  <td className="px-4 py-3 text-right">
                    <button
                      onClick={() => setDeleteTarget(s.id)}
                      className="text-xs text-zinc-400 hover:text-red-600 transition-colors"
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {showForm && <NewSquadForm agents={agents} onClose={() => setShowForm(false)} />}
      {deleteTarget && (
        <ConfirmDialog
          title="确认删除"
          message="确定要删除此 Squad 吗？"
          onConfirm={() => deleteSquad.mutate(deleteTarget, { onSuccess: () => setDeleteTarget(null) })}
          onClose={() => setDeleteTarget(null)}
          loading={deleteSquad.isPending}
        />
      )}
    </div>
  );
}

function NewSquadForm({
  agents,
  onClose,
}: {
  agents?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const createSquad = useCreateSquad();
  const addMember = useAddSquadMember();
  const [name, setName] = useState("");
  // 成员是第一概念：先勾选团队成员，再从中指定 leader / reviewer 角色。
  const [members, setMembers] = useState<string[]>([]);
  const [leaderId, setLeaderId] = useState("");
  const [reviewerId, setReviewerId] = useState("");
  const [description, setDescription] = useState("");
  const [instructions, setInstructions] = useState("");

  const memberAgents = (agents ?? []).filter((a) => members.includes(a.id));
  const toggleMember = (id: string) => {
    const next = members.includes(id) ? members.filter((m) => m !== id) : [...members, id];
    setMembers(next);
    if (!next.includes(leaderId)) setLeaderId("");
    if (!next.includes(reviewerId)) setReviewerId("");
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createSquad.mutate(
      { name, leader_id: leaderId, description, instructions },
      {
        onSuccess: (sq) => {
          // Add every member; the picked reviewer gets role=reviewer
          // (决策 4-4: the squad owns the who-reviews rule; the platform
          // pulls role=reviewer members into review runs).
          const rest = members.filter((m) => m !== leaderId);
          const enqueue = (i: number) => {
            if (i >= rest.length) {
              onClose();
              return;
            }
            const m = rest[i];
            addMember.mutate(
              { squadId: sq.id, member_type: "agent", member_id: m, role: m === reviewerId ? "reviewer" : "member" },
              { onSuccess: () => enqueue(i + 1), onError: () => enqueue(i + 1) }
            );
          };
          enqueue(0);
        },
        onError: () => onClose(),
      }
    );
  };

  return (
    <Dialog
      title="新建 Squad"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="squad-form" disabled={createSquad.isPending}>
            {createSquad.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="squad-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称" hint="必填">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required placeholder="Squad 名称…" />
        </Field>
        <Field label="成员" hint="三角色分工：leader = 协调者（拆解任务并分派给成员，不自己实现全部）；执行者 = 被 leader 分派干活的成员；reviewer = 审查者（进审批时被平台自动拉去审查）。先选成员（至少 leader），再从成员中指定 leader 和 reviewer">
          <div className="border border-zinc-200 rounded-lg divide-y divide-zinc-100 max-h-48 overflow-y-auto">
            {(agents ?? []).map((a) => (
              <label key={a.id} className="flex items-center gap-2 px-3 py-2 text-sm cursor-pointer hover:bg-zinc-50">
                <input type="checkbox" checked={members.includes(a.id)} onChange={() => toggleMember(a.id)} />
                {a.name}
              </label>
            ))}
          </div>
        </Field>
        <Field label="Leader" hint="必填，从成员中选">
          <select value={leaderId} onChange={(e) => setLeaderId(e.target.value)} className={inputCls} required>
            <option value="">选择…</option>
            {memberAgents.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        <Field label="审核者（reviewer）" hint="可选——squad 任务进审批时，平台会自动拉审核者审查（决策 4-4）">
          <select value={reviewerId} onChange={(e) => setReviewerId(e.target.value)} className={inputCls}>
            <option value="">无（不需要 agent 审查）</option>
            {memberAgents.filter((a) => a.id !== leaderId).map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        <Field label="描述">
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} className={inputCls} rows={2} />
        </Field>
        <Field label="Instructions" hint="leader 运行时会注入这些 instructions">
          <textarea value={instructions} onChange={(e) => setInstructions(e.target.value)} className={inputCls} rows={3} placeholder="Squad 工作说明…" />
        </Field>
        {createSquad.isError && (
          <p className="text-sm text-red-500">{String(createSquad.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
