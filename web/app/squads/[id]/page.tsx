"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { useSquads, useAgents, useSquadMembers, useAddSquadMember, useUpdateSquadInstructions, useUpdateSquadMember, useGoalEvents } from "@/lib/queries";
import { Button, PageHeader, Empty, Dialog, Field, inputCls } from "@/components/ui";
import type { SquadMember } from "@/lib/types";

export default function SquadDetailPage() {
  useGoalEvents();
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data: squads } = useSquads();
  const { data: agents } = useAgents();
  const { data: members, isLoading } = useSquadMembers(id);
  const addMember = useAddSquadMember();
  const updateInstructions = useUpdateSquadInstructions();
  const updateMemberRole = useUpdateSquadMember();
  const [showAdd, setShowAdd] = useState(false);
  const [editingInstructions, setEditingInstructions] = useState(false);
  const [instructionsText, setInstructionsText] = useState("");
  const [editingRole, setEditingRole] = useState<{ memberId: string; role: string } | null>(null);

  const squad = squads?.find((s) => s.id === id);
  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;

  if (!squad) {
    if (squads === undefined) return <div className="p-8 text-sm text-zinc-400">加载中…</div>;
    return <div className="p-8 text-sm text-zinc-400">找不到 Squad。</div>;
  }

  return (
    <div className="p-8 max-w-4xl space-y-5">
      <div>
        <Link href="/squads" className="text-sm text-zinc-400 hover:text-zinc-700 hover:underline">
          ← 返回列表
        </Link>
        <h1 className="text-lg font-semibold text-zinc-900 mt-3">{squad.name}</h1>
      </div>

      {/* Squad info */}
      <div className="bg-white rounded-xl border border-zinc-200 p-5 space-y-3">
        <div className="grid grid-cols-2 gap-3 text-sm">
          <div>
            <span className="text-zinc-400">Leader：</span>
            <span className="text-zinc-700">{agentName(squad.leader_id)}</span>
          </div>
          <div>
            <span className="text-zinc-400">创建时间：</span>
            <span className="text-zinc-700">{new Date(squad.created_at).toLocaleString()}</span>
          </div>
        </div>
        {squad.description && (
          <div>
            <span className="text-xs text-zinc-400 block mb-1">描述</span>
            <p className="text-sm text-zinc-600">{squad.description}</p>
          </div>
        )}
      </div>

      {/* Instructions editor */}
      <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
        <div className="px-4 py-2.5 border-b border-zinc-100 flex items-center justify-between">
          <span className="text-xs font-medium text-zinc-500 uppercase tracking-wide">
            Instructions
          </span>
          {!editingInstructions ? (
            <Button variant="outline" onClick={() => {
              setInstructionsText(squad.instructions || "");
              setEditingInstructions(true);
            }}>
              编辑
            </Button>
          ) : (
            <div className="flex gap-2">
              <Button variant="outline" onClick={() => setEditingInstructions(false)}>取消</Button>
              <Button
                onClick={() => {
                  updateInstructions.mutate(
                    { squadId: id, instructions: instructionsText },
                    { onSuccess: () => setEditingInstructions(false) }
                  );
                }}
                disabled={updateInstructions.isPending}
              >
                {updateInstructions.isPending ? "…" : "保存"}
              </Button>
            </div>
          )}
        </div>
        <div className="p-4">
          {!editingInstructions ? (
            <div>
              {squad.instructions ? (
                <div className="text-sm text-zinc-600 bg-zinc-50 rounded-lg p-4 whitespace-pre-wrap max-h-64 overflow-y-auto">
                  {squad.instructions}
                </div>
              ) : (
                <p className="text-sm text-zinc-400">
                  未配置 Instructions。在这里用自然语言描述团队的工作方式：角色分工、工作约定、交接流程等。
                  所有 squad leader 执行任务时都会在 prompt 中看到这些内容。
                </p>
              )}
            </div>
          ) : (
            <div className="space-y-3">
              <Field label="Instructions（Markdown）" hint="用自然语言描述团队的工作方式。例如：角色分工、工作约定、交接流程、注意事项等。Leader agent 执行任务时会注入到 prompt 中。">
                <textarea
                  value={instructionsText}
                  onChange={(e) => setInstructionsText(e.target.value)}
                  className={inputCls}
                  rows={16}
                  placeholder={`## 团队工作方式

### 角色分工
- **开发者 (Alice)**：负责实现功能、写代码
- **审查者 (Bob)**：负责代码审查
- **测试者 (Carol)**：负责验证功能

### 工作约定
1. 开发者完成任务后，@mention 审查者进行代码审查
2. 审查者通过则 @mention 测试者，需修改则 @mention 回开发者
3. 测试通过后，@mention 开发者标记目标完成

### 注意事项
- 每个交接都要在评论中说明完成了什么、下一步需要什么`}
                />
              </Field>
              {updateInstructions.isError && (
                <p className="text-sm text-red-500">{String(updateInstructions.error)}</p>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Members */}
      <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
        <div className="px-4 py-2.5 border-b border-zinc-100 flex items-center justify-between">
          <span className="text-xs font-medium text-zinc-500 uppercase tracking-wide">
            成员{members && members.length > 0 && `（${members.length}）`}
          </span>
          <Button onClick={() => setShowAdd(true)}>+ 添加</Button>
        </div>
        <div className="p-4">
          {isLoading ? (
            <div className="text-sm text-zinc-400 text-center py-8">加载中…</div>
          ) : !members || members.length === 0 ? (
            <Empty>暂无成员。</Empty>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-zinc-100 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                  <th className="px-3 py-2">类型</th>
                  <th className="px-3 py-2">名称</th>
                  <th className="px-3 py-2">角色</th>
                  <th className="px-3 py-2">添加时间</th>
                </tr>
              </thead>
              <tbody>
                {members.map((m: SquadMember) => (
                  <tr key={m.id} className="border-b border-zinc-50">
                    <td className="px-3 py-2">
                      <span className={`px-2 py-0.5 text-xs rounded ${m.member_type === "agent" ? "bg-blue-50 text-blue-700" : "bg-zinc-100 text-zinc-600"}`}>
                        {m.member_type}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-zinc-700">
                      {m.member_type === "agent" ? (agentName(m.member_id) || m.member_id) : m.member_id}
                    </td>
                    <td className="px-3 py-2 text-zinc-500">
                      <span className="flex items-center gap-1">
                        {m.role || "-"}
                        <button
                          onClick={() => setEditingRole({ memberId: m.member_id, role: m.role || "" })}
                          className="text-[10px] text-zinc-400 hover:text-blue-500 ml-1"
                          title="编辑角色"
                        >
                          ✎
                        </button>
                      </span>
                    </td>
                    <td className="px-3 py-2 text-zinc-400 text-xs">{new Date(m.created_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {/* Add member dialog */}
      {showAdd && (
        <AddMemberDialog
          squadId={id}
          agents={agents}
          onClose={() => setShowAdd(false)}
        />
      )}

      {/* Edit role dialog */}
      {editingRole && (
        <Dialog
          title="编辑角色"
          onClose={() => setEditingRole(null)}
          footer={
            <>
              <Button variant="outline" onClick={() => setEditingRole(null)}>取消</Button>
              <Button
                onClick={() => {
                  updateMemberRole.mutate(
                    { squadId: id, memberId: editingRole.memberId, role: editingRole.role },
                    { onSuccess: () => setEditingRole(null) }
                  );
                }}
                disabled={updateMemberRole.isPending}
              >
                {updateMemberRole.isPending ? "…" : "保存"}
              </Button>
            </>
          }
        >
          <Field label="角色名称" hint="角色用于标识成员在团队中的职责，如：开发者、审查者、测试者。">
            <input
              value={editingRole.role}
              onChange={(e) => setEditingRole({ ...editingRole, role: e.target.value })}
              className={inputCls}
              placeholder="例如：开发者、审查者、测试者"
            />
          </Field>
          {updateMemberRole.isError && (
            <p className="text-sm text-red-500">{String(updateMemberRole.error)}</p>
          )}
        </Dialog>
      )}
    </div>
  );
}

function AddMemberDialog({
  squadId,
  agents,
  onClose,
}: {
  squadId: string;
  agents?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const addMember = useAddSquadMember();
  const [memberType, setMemberType] = useState("agent");
  const [memberId, setMemberId] = useState("");
  const [role, setRole] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    addMember.mutate(
      { squadId, member_type: memberType, member_id: memberId, role },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="添加成员"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="add-member-form" disabled={addMember.isPending}>
            {addMember.isPending ? "…" : "添加"}
          </Button>
        </>
      }
    >
      <form id="add-member-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="成员类型">
          <select value={memberType} onChange={(e) => { setMemberType(e.target.value); setMemberId(""); }} className={inputCls}>
            <option value="agent">Agent</option>
            <option value="human">Human</option>
          </select>
        </Field>
        {memberType === "agent" && (
          <Field label="选择 Agent">
            <select value={memberId} onChange={(e) => setMemberId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {agents?.map((a) => (
                <option key={a.id} value={a.id}>{a.name}</option>
              ))}
            </select>
          </Field>
        )}
        {memberType === "human" && (
          <Field label="Human ID">
            <input value={memberId} onChange={(e) => setMemberId(e.target.value)} className={inputCls} required placeholder="human id…" />
          </Field>
        )}
        <Field label="角色">
          <input value={role} onChange={(e) => setRole(e.target.value)} className={inputCls} placeholder="开发者、审查者 等" />
        </Field>
        {addMember.isError && (
          <p className="text-sm text-red-500">{String(addMember.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
