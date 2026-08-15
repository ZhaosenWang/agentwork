"use client";

import { useMachines } from "@/lib/queries";
import { PageHeader, Empty } from "@/components/ui";
import type { ProbeCLI } from "@/lib/types";

// parseCLIs decodes the machine's probe report (best-effort — bad JSON
// shows as "未知" rather than crashing the page).
function parseCLIs(raw: string): ProbeCLI[] {
  try {
    const v = JSON.parse(raw);
    return Array.isArray(v) ? (v as ProbeCLI[]) : [];
  } catch {
    return [];
  }
}

export default function MachinesPage() {
  const { data: machines, isLoading } = useMachines();

  return (
    <div className="p-6 max-w-4xl mx-auto space-y-4">
      <PageHeader title="机器" />
      <p className="text-sm text-gray-500 -mt-3">
        远端主机执行 `agentwork connect` 后注册到这里——连接的机器、探测到的 agent CLI 一览。
      </p>

      {isLoading ? (
        <p className="text-gray-500 text-sm">加载中…</p>
      ) : !machines?.length ? (
        <Empty>
          还没有注册的机器——在远端主机上安装 agentwork 并运行 `agentwork connect`。
        </Empty>
      ) : (
        <div className="space-y-3">
          {machines.map((m) => {
            const clis = parseCLIs(m.probed_clis);
            return (
              <div key={m.id} className="rounded-xl border border-zinc-200/80 bg-white shadow-sm p-4 space-y-2">
                <div className="flex items-center gap-2 flex-wrap">
                  <span className="font-medium text-sm text-zinc-900">{m.name}</span>
                  <span className="text-xs text-zinc-400 font-mono">{m.hostname}</span>
                  <span className="ml-auto flex items-center gap-2">
                    {m.status === "connected" ? (
                      <span className="inline-flex items-center gap-1.5 px-2.5 py-0.5 text-xs font-medium rounded-full bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200">
                        <span className="h-1.5 w-1.5 rounded-full bg-emerald-500" />在线
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-1.5 px-2.5 py-0.5 text-xs font-medium rounded-full bg-zinc-100 text-zinc-500 ring-1 ring-zinc-200">
                        <span className="h-1.5 w-1.5 rounded-full bg-zinc-400" />离线
                      </span>
                    )}
                    <span className="text-xs text-zinc-400" title={m.last_seen_at}>
                      {m.last_seen_at ? new Date(m.last_seen_at).toLocaleString("zh-CN") : "—"}
                    </span>
                  </span>
                </div>
                {clis.length > 0 ? (
                  <div className="space-y-1">
                    <p className="text-xs font-medium text-zinc-500">探测到的 agent CLI</p>
                    {clis.map((c) => (
                      <div key={c.name} className="text-xs text-zinc-600 flex items-center gap-2 flex-wrap">
                        <span className="font-mono font-medium">{c.name}</span>
                        {c.version && <span className="text-zinc-400">{c.version}</span>}
                        {c.skills_dir && <span className="text-zinc-400 font-mono">{c.skills_dir}</span>}
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="text-xs text-zinc-400">未探测到 agent CLI</p>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
