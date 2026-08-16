"use client";

import { useState } from "react";
import { useRuntimes, useDeleteRuntime, useMachines, useGoalEvents } from "@/lib/queries";
import { Button, PageHeader, Empty } from "@/components/ui";
import type { Runtime } from "@/lib/types";

function RuntimeRow({ rt, machineName, onDelete }: { rt: Runtime; machineName: string; onDelete: (id: string) => void }) {
  const [confirming, setConfirming] = useState(false);
  return (
    <tr className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
      <td className="px-4 py-3 font-medium text-zinc-900">{rt.name}</td>
      <td className="px-4 py-3">
        <span className="px-2 py-0.5 text-xs rounded bg-emerald-50 text-emerald-700">{machineName || "—"}</span>
      </td>
      <td className="px-4 py-3">
        {rt.status === "absent" ? (
          <span className="px-2 py-0.5 text-xs rounded bg-amber-50 text-amber-700">CLI 已不在机器上</span>
        ) : (
          <span className="px-2 py-0.5 text-xs rounded bg-zinc-50 text-zinc-500">active</span>
        )}
      </td>
      <td className="px-4 py-3 text-zinc-600 font-mono text-xs">{(rt.args ?? []).join(" ")}</td>
      <td className="px-4 py-3 text-zinc-400">{new Date(rt.created_at).toLocaleString()}</td>
      <td className="px-4 py-3 text-right">
        {confirming ? (
          <span className="text-xs space-x-2">
            <button
              className="text-red-500 hover:text-red-700 font-medium"
              onClick={() => { onDelete(rt.id); setConfirming(false); }}
            >
              确认删除
            </button>
            <button className="text-zinc-400 hover:text-zinc-600" onClick={() => setConfirming(false)}>取消</button>
          </span>
        ) : (
          <button
            className="text-xs text-zinc-400 hover:text-red-500 transition-colors"
            onClick={() => setConfirming(true)}
          >
            删除
          </button>
        )}
      </td>
    </tr>
  );
}

export default function RuntimesPage() {
  useGoalEvents();
  const { data: runtimes, isLoading } = useRuntimes();
  const { data: machines } = useMachines();
  const del = useDeleteRuntime();

  const machineName = (id: string) => machines?.find((m) => m.id === id)?.name ?? id;

  return (
    <div className="p-6 max-w-5xl mx-auto">
      <PageHeader title="Runtime" />
      <p className="text-sm text-gray-500 -mt-3 mb-4">
        Runtime 由远端机器上的 `agentwork connect` 探测自动生成（&lt;cli&gt;@&lt;机器名&gt;）——不需要手工创建。
      </p>

      {isLoading ? (
        <p className="text-gray-500 text-sm">加载中…</p>
      ) : !runtimes?.length ? (
        <Empty>
          还没有 Runtime——在机器上运行 `agentwork connect`，探测到的 agent CLI 会自动注册到这里。
        </Empty>
      ) : (
        <div className="bg-white rounded-2xl border border-zinc-200/80 shadow-sm overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-xs text-zinc-400 border-b border-zinc-100">
                <th className="px-4 py-2.5 font-medium">名称</th>
                <th className="px-4 py-2.5 font-medium">机器</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">启动参数（acp_spawn）</th>
                <th className="px-4 py-2.5 font-medium">创建时间</th>
                <th className="px-4 py-2.5 font-medium text-right">操作</th>
              </tr>
            </thead>
            <tbody>
              {runtimes.map((rt) => (
                <RuntimeRow key={rt.id} rt={rt} machineName={machineName(rt.machine_id)} onDelete={(id) => del.mutate(id)} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {del.isError && <p className="mt-2 text-sm text-red-500">{String(del.error)}</p>}
    </div>
  );
}
