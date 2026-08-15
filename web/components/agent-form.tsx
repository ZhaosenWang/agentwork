"use client";

import { useState } from "react";
import { useCreateAgent, useUpdateAgent, useRuntimes, useSkills } from "@/lib/queries";
import type { Agent, McpServer } from "@/lib/types";
import { Button, Dialog, Field, inputCls } from "@/components/ui";

// KVRows is a name-value pair list editor (env vars / headers) — a "+"
// appends an empty row, ✕ removes one. Rows carry {name, value} (the acp
// wire shape), so the form never touches JSON.
function KVRows({ rows, onChange, keyPlaceholder, valuePlaceholder }: {
  rows: { name: string; value: string }[];
  onChange: (rows: { name: string; value: string }[]) => void;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
}) {
  return (
    <div className="space-y-1.5">
      {rows.map((r, i) => (
        <div key={i} className="flex gap-1.5">
          <input
            value={r.name}
            onChange={(e) => onChange(rows.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))}
            className={`${inputCls} flex-1 min-w-0 font-mono !py-1 text-xs`}
            placeholder={keyPlaceholder ?? "名称"}
          />
          <input
            value={r.value}
            onChange={(e) => onChange(rows.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)))}
            className={`${inputCls} flex-1 min-w-0 font-mono !py-1 text-xs`}
            placeholder={valuePlaceholder ?? "值"}
          />
          <button
            type="button"
            onClick={() => onChange(rows.filter((_, j) => j !== i))}
            className="text-zinc-400 hover:text-red-500 shrink-0"
            title="删除"
          >✕</button>
        </div>
      ))}
      <button type="button" className="text-indigo-600 hover:text-indigo-700 text-xs" onClick={() => onChange([...rows, { name: "", value: "" }])}>
        + 添加
      </button>
    </div>
  );
}

// AgentForm: 新建/编辑 Agent。基础配置平铺，运维项（并发/环境变量/额外
// MCP 服务器）收进"高级设置"折叠区——env 是键值对行，MCP 服务器是结构化
// 条目，都不要求手写 JSON。
export function AgentForm({ agent, onClose }: { agent?: Agent; onClose: () => void }) {
  const create = useCreateAgent();
  const update = useUpdateAgent();
  const { data: runtimes } = useRuntimes();
  const [name, setName] = useState(agent?.name ?? "");
  const [runtimeId, setRuntimeId] = useState(agent?.runtime_id ?? "");
  const [systemPrompt, setSystemPrompt] = useState(agent?.system_prompt ?? "");
  const [model, setModel] = useState(agent?.model ?? "");
  const [envRows, setEnvRows] = useState<{ name: string; value: string }[]>(
    agent?.env ? Object.entries(agent.env).map(([name, value]) => ({ name, value })) : []
  );
  const [mcpRows, setMcpRows] = useState<McpServer[]>(agent?.mcp_servers ?? []);
  const [maxConcurrent, setMaxConcurrent] = useState(String(agent?.max_concurrent ?? 1));
  const [skills, setSkills] = useState<string[]>(agent?.skills ?? []);
  const { data: skillLib } = useSkills();

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const parsedEnv: Record<string, string> = {};
    for (const r of envRows) {
      if (r.name.trim()) parsedEnv[r.name.trim()] = r.value;
    }
    const body = {
      name,
      description: agent?.description ?? "",
      runtime_id: runtimeId,
      system_prompt: systemPrompt,
      model,
      env: parsedEnv,
      // acp wire semantics: type "" = stdio (not the string "stdio").
      mcp_servers: mcpRows
        .filter((m) => m.name.trim())
        .map((m) => ({ ...m, type: m.type === "stdio" ? "" : m.type })),
      max_concurrent: parseInt(maxConcurrent) || 1,
      skills,
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
      wide
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

        <Field label="Skills（平台技能库）" hint="勾选的 skill 会下发到该 agent 所在机器（agentwork-<名称>/ 命名空间）">
          {skillLib && skillLib.length > 0 ? (
            <div className="space-y-1">
              {skillLib.map((sk) => (
                <label key={sk.id} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={skills.includes(sk.id)}
                    onChange={(e) =>
                      setSkills((prev) =>
                        e.target.checked ? [...prev, sk.id] : prev.filter((x) => x !== sk.id)
                      )
                    }
                  />
                  <span className="font-medium">{sk.name}</span>
                  {sk.description && <span className="text-xs text-zinc-500">— {sk.description}</span>}
                </label>
              ))}
            </div>
          ) : (
            <p className="text-xs text-zinc-400">技能库为空——在「Skills」页面上传 skill 包</p>
          )}
        </Field>

        <Field label="Model" hint="留空用 runtime 默认">
          <input value={model} onChange={(e) => setModel(e.target.value)} className={inputCls} />
        </Field>

        <details className="text-xs">
          <summary className="cursor-pointer text-zinc-400 hover:text-zinc-600">高级设置</summary>
          <div className="mt-2 space-y-4">
            <Field
              label="同时执行的任务数"
              hint="同一个 agent 同时能跑几个任务。保持 1 = 一次只干一个活（推荐）；多个 goal 都派给它且想并行时再调大，注意模型 API 可能限流。不同 agent 之间天然并行，无需调这个。"
            >
              <input value={maxConcurrent} onChange={(e) => setMaxConcurrent(e.target.value)} className={inputCls} type="number" min="1" />
            </Field>

            <Field label="环境变量" hint="运行时注入给 agent 进程（覆盖同名平台变量）">
              <KVRows rows={envRows} onChange={setEnvRows} keyPlaceholder="KEY" valuePlaceholder="VALUE" />
            </Field>

            <Field label="额外 MCP 服务器" hint="给这个 agent 挂它自己的工具（浏览器、数据库等）；平台的 workspace 工具始终自带">
              <div className="space-y-2">
                {mcpRows.map((m, i) => {
                  // Display type: the wire uses "" for stdio — both "" and
                  // the legacy "stdio" string render as the stdio option.
                  const t = m.type === "http" || m.type === "sse" ? m.type : "stdio";
                  return (
                    <div key={i} className="border border-zinc-200 rounded-lg p-2 space-y-2">
                      <div className="flex gap-2">
                        <input
                          value={m.name}
                          onChange={(e) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))}
                          className={`${inputCls} flex-1 min-w-0`}
                          placeholder="名称（如 browser）"
                        />
                        {/* inputCls 自带 w-full，与固定宽度类冲突——用容器定宽 */}
                        <div className="w-32 shrink-0">
                          <select
                            value={t}
                            onChange={(e) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, type: e.target.value } : x)))}
                            className={inputCls}
                          >
                            <option value="http">HTTP</option>
                            <option value="sse">SSE</option>
                            <option value="stdio">stdio 命令</option>
                          </select>
                        </div>
                        <button
                          type="button"
                          onClick={() => setMcpRows(mcpRows.filter((_, j) => j !== i))}
                          className="text-zinc-400 hover:text-red-500 shrink-0"
                          title="删除"
                        >✕</button>
                      </div>
                      {t === "stdio" ? (
                        <div className="space-y-2">
                          <div className="flex gap-2">
                            <input
                              value={m.command ?? ""}
                              onChange={(e) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, command: e.target.value } : x)))}
                              className={`${inputCls} flex-1 min-w-0 font-mono`}
                              placeholder="命令（如 npx）"
                            />
                            <input
                              value={(m.args ?? []).join(" ")}
                              onChange={(e) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, args: e.target.value.split(/\s+/).filter(Boolean) } : x)))}
                              className={`${inputCls} flex-1 min-w-0 font-mono`}
                              placeholder="参数（空格分隔）"
                            />
                          </div>
                          <details>
                            <summary className="cursor-pointer text-zinc-400 hover:text-zinc-600 text-[11px]">环境变量</summary>
                            <div className="mt-1">
                              <KVRows
                                rows={m.env ?? []}
                                onChange={(rows) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, env: rows } : x)))}
                                keyPlaceholder="KEY"
                                valuePlaceholder="VALUE"
                              />
                            </div>
                          </details>
                        </div>
                      ) : (
                        <div className="space-y-2">
                          <input
                            value={m.url ?? ""}
                            onChange={(e) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, url: e.target.value } : x)))}
                            className={`${inputCls} w-full min-w-0 font-mono`}
                            placeholder="http://localhost:9222/mcp"
                          />
                          <details>
                            <summary className="cursor-pointer text-zinc-400 hover:text-zinc-600 text-[11px]">请求头</summary>
                            <div className="mt-1">
                              <KVRows
                                rows={m.headers ?? []}
                                onChange={(rows) => setMcpRows(mcpRows.map((x, j) => (j === i ? { ...x, headers: rows } : x)))}
                                keyPlaceholder="Header"
                                valuePlaceholder="值"
                              />
                            </div>
                          </details>
                        </div>
                      )}
                    </div>
                  );
                })}
                <button type="button" className="text-indigo-600 hover:text-indigo-700" onClick={() => setMcpRows([...mcpRows, { name: "", type: "http" }])}>
                  + 添加服务器
                </button>
              </div>
            </Field>
          </div>
        </details>

        {(create.isError || update.isError) && (
          <p className="text-sm text-red-500">{String(create.error ?? update.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
