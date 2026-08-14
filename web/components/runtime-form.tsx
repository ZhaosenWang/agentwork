"use client";

import { useState } from "react";
import { useCreateRuntime } from "@/lib/queries";
import { Button, Dialog, Field, inputCls } from "@/components/ui";

export function RuntimeForm({ onClose }: { onClose: () => void }) {
  const create = useCreateRuntime();
  const [name, setName] = useState("");
  const [transport, setTransport] = useState("stdio");
  const [provider, setProvider] = useState("acp");
  const [executable, setExecutable] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [args, setArgs] = useState("");
  const [envRows, setEnvRows] = useState<{ key: string; value: string }[]>([]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const parsedEnv: Record<string, string> = {};
    for (const r of envRows) {
      if (r.key.trim()) parsedEnv[r.key.trim()] = r.value;
    }
    create.mutate(
      {
        name,
        transport,
        provider,
        executable,
        endpoint,
        args: args.trim() ? args.trim().split(/\s+/).filter(Boolean) : [],
        env: parsedEnv,
      },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="新建 Runtime"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="runtime-form" disabled={create.isPending}>
            {create.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="runtime-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} placeholder="openagent-cli" required />
        </Field>

        <Field label="Transport">
          <select value={transport} onChange={(e) => setTransport(e.target.value)} className={inputCls}>
            <option value="stdio">stdio</option>
            <option value="ws">ws</option>
            <option value="tcp">tcp</option>
          </select>
        </Field>

        <Field label="Provider" hint="协议类型（agent 使用的通信协议）">
          <select value={provider} onChange={(e) => setProvider(e.target.value)} className={inputCls}>
            <option value="acp">acp</option>
            <option value="jsonl">jsonl</option>
            <option value="jsonrpc">jsonrpc</option>
          </select>
        </Field>

        {transport === "stdio" ? (
          <Field label="Executable">
            <input value={executable} onChange={(e) => setExecutable(e.target.value)} className={inputCls} placeholder="/path/to/openagent-cli" required />
          </Field>
        ) : (
          <Field label="Endpoint">
            <input value={endpoint} onChange={(e) => setEndpoint(e.target.value)} className={inputCls} placeholder={transport === "ws" ? "ws://host:port" : "host:port"} required />
          </Field>
        )}

        {transport === "stdio" && (
          <Field label="启动参数" hint="空格分隔，如 serve --acp">
            <input value={args} onChange={(e) => setArgs(e.target.value)} className={`${inputCls} font-mono`} placeholder="serve --acp" />
          </Field>
        )}

        {transport === "stdio" && (
          <Field label="环境变量" hint="注入给 agent 进程">
            <div className="space-y-2">
              {envRows.map((r, i) => (
                <div key={i} className="flex gap-2">
                  <input
                    value={r.key}
                    onChange={(e) => setEnvRows(envRows.map((x, j) => (j === i ? { ...x, key: e.target.value } : x)))}
                    className={`${inputCls} flex-1 font-mono`}
                    placeholder="KEY"
                  />
                  <input
                    value={r.value}
                    onChange={(e) => setEnvRows(envRows.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)))}
                    className={`${inputCls} flex-1 font-mono`}
                    placeholder="VALUE"
                  />
                  <button
                    type="button"
                    onClick={() => setEnvRows(envRows.filter((_, j) => j !== i))}
                    className="text-zinc-400 hover:text-red-500 shrink-0"
                    title="删除"
                  >✕</button>
                </div>
              ))}
              <button type="button" className="text-indigo-600 hover:text-indigo-700" onClick={() => setEnvRows([...envRows, { key: "", value: "" }])}>
                + 添加变量
              </button>
            </div>
          </Field>
        )}

        {create.isError && (
          <p className="text-sm text-red-500">{String(create.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
