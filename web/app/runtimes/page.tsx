"use client";

import { useState } from "react";
import { useRuntimes, useDeleteRuntime, useTestRuntime, useGoalEvents } from "@/lib/queries";
import { RuntimeForm } from "@/components/runtime-form";
import { Button, PageHeader, Empty } from "@/components/ui";
import type { Runtime, RuntimeTestResult } from "@/lib/types";

function TestStatus({ result, testing }: { result?: RuntimeTestResult; testing: boolean }) {
  if (testing) return <span className="text-xs text-zinc-400">测试中…</span>;
  if (!result) return null;
  if (result.ok) {
    return (
      <span className="text-xs text-emerald-600" title={result.details}>
        ✓ 可用 ({result.latency_ms}ms){result.details ? ` · ${result.details}` : ""}
      </span>
    );
  }
  return (
    <span className="text-xs text-red-500" title={result.error}>
      ✗ {result.error}
    </span>
  );
}

function RuntimeRow({ rt, onDelete }: { rt: Runtime; onDelete: (id: string) => void }) {
  const test = useTestRuntime();
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<RuntimeTestResult | undefined>(undefined);

  const handleTest = () => {
    setTesting(true);
    test.mutateAsync(rt.id)
      .then(setResult)
      .catch(() => setResult({ ok: false, error: "请求失败", latency_ms: 0 }))
      .finally(() => setTesting(false));
  };

  return (
    <tr className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
      <td className="px-4 py-3 font-medium text-zinc-900">{rt.name}</td>
      <td className="px-4 py-3">
        <span className="px-2 py-0.5 text-xs rounded bg-zinc-100 text-zinc-600">{rt.transport}</span>
      </td>
      <td className="px-4 py-3">
        <span className="px-2 py-0.5 text-xs rounded bg-blue-50 text-blue-700">{rt.provider}</span>
      </td>
      <td className="px-4 py-3 text-zinc-600 font-mono text-xs">
        {rt.transport === "stdio" ? rt.executable : rt.endpoint}
      </td>
      <td className="px-4 py-3">
        <TestStatus result={result} testing={testing} />
      </td>
      <td className="px-4 py-3 text-zinc-400">{new Date(rt.created_at).toLocaleString()}</td>
      <td className="px-4 py-3 text-right space-x-3 whitespace-nowrap">
        <button
          onClick={handleTest}
          disabled={testing}
          className="text-xs text-indigo-500 hover:text-indigo-700 transition-colors disabled:opacity-50"
        >
          测试
        </button>
        <button
          onClick={() => onDelete(rt.id)}
          className="text-xs text-zinc-400 hover:text-red-600 transition-colors"
        >
          删除
        </button>
      </td>
    </tr>
  );
}

export default function RuntimesPage() {
  useGoalEvents();
  const { data: runtimes, isLoading } = useRuntimes();
  const del = useDeleteRuntime();
  const [showForm, setShowForm] = useState(false);

  return (
    <div className="p-8">
      <PageHeader
        title="Runtime"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {showForm && <RuntimeForm onClose={() => setShowForm(false)} />}

      {isLoading && <p className="text-sm text-zinc-400">加载中…</p>}

      {runtimes && runtimes.length > 0 && (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">名称</th>
                <th className="px-4 py-3">Transport</th>
                <th className="px-4 py-3">Provider</th>
                <th className="px-4 py-3">可执行 / Endpoint</th>
                <th className="px-4 py-3">连通性</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-28"></th>
              </tr>
            </thead>
            <tbody>
              {runtimes.map((rt: Runtime) => (
                <RuntimeRow key={rt.id} rt={rt} onDelete={del.mutate} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {runtimes && runtimes.length === 0 && (
        <Empty>还没有 runtime。点「+ 新建」创建一个。</Empty>
      )}
    </div>
  );
}
