"use client";

import { useState } from "react";
import { useCreateAgent, useUpdateAgent, useRuntimes } from "@/lib/queries";
import type { Agent } from "@/lib/types";
import { Button, Dialog, Field, inputCls } from "@/components/ui";

export function AgentForm({ agent, onClose }: { agent?: Agent; onClose: () => void }) {
  const create = useCreateAgent();
  const update = useUpdateAgent();
  const { data: runtimes } = useRuntimes();
  const [name, setName] = useState(agent?.name ?? "");
  const [runtimeId, setRuntimeId] = useState(agent?.runtime_id ?? "");
  const [systemPrompt, setSystemPrompt] = useState(agent?.system_prompt ?? "");
  const [model, setModel] = useState(agent?.model ?? "");
  const [env, setEnv] = useState(agent?.env ? JSON.stringify(agent.env, null, 2) : "{}");
  const [maxConcurrent, setMaxConcurrent] = useState(String(agent?.max_concurrent ?? 1));

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    let parsedEnv: Record<string, string> = {};
    try { parsedEnv = JSON.parse(env || "{}"); } catch { /* keep default */ }
    const body = {
      name,
      description: agent?.description ?? "",
      runtime_id: runtimeId,
      system_prompt: systemPrompt,
      model,
      env: parsedEnv,
      max_concurrent: parseInt(maxConcurrent) || 1,
    };
    if (agent) {
      update.mutate({ id: agent.id, ...body }, { onSuccess: onClose });
    } else {
      create.mutate(body, { onSuccess: onClose });
    }
  };

  return (
    <Dialog
      title={agent ? `编辑 Agent：${agent.name}` : "新建 Agent"}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="agent-form" disabled={create.isPending || update.isPending}>
            {(create.isPending || update.isPending) ? "保存中…" : agent ? "保存" : "创建"}
          </Button>
        </>
      }
    >
      <form id="agent-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required />
        </Field>

        <Field label="Runtime">
          <select value={runtimeId} onChange={(e) => setRuntimeId(e.target.value)} className={inputCls} required>
            <option value="">选择…</option>
            {runtimes?.map((rt) => (
              <option key={rt.id} value={rt.id}>{rt.name}</option>
            ))}
          </select>
        </Field>

        <Field label="System Prompt">
          <textarea value={systemPrompt} onChange={(e) => setSystemPrompt(e.target.value)} className={`${inputCls} font-mono`} rows={3} />
        </Field>

        <Field label="Model" hint="留空用 runtime 默认">
          <input value={model} onChange={(e) => setModel(e.target.value)} className={inputCls} />
        </Field>

        <Field label="Env (JSON)">
          <input value={env} onChange={(e) => setEnv(e.target.value)} className={`${inputCls} font-mono`} placeholder="{}" />
        </Field>

        <Field label="最大并发">
          <input value={maxConcurrent} onChange={(e) => setMaxConcurrent(e.target.value)} className={inputCls} type="number" min="1" />
        </Field>

        {create.isError && (
          <p className="text-sm text-red-500">{String(create.error)}</p>
        )}
      </form>
    </Dialog>
  );
}