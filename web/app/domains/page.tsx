"use client";

import { useState } from "react";
import {
  useDomains, useDomain, useAgents, useCreateDomain, useCompileDomainPolicy,
  useFreezeDomainChecks, useGoalEvents,
} from "@/lib/queries";
import { Button, Dialog, Field, inputCls, PageHeader, Empty, Badge } from "@/components/ui";
import type { Domain, Checks } from "@/lib/types";

export default function DomainsPage() {
  useGoalEvents();
  const { data: domains, isLoading } = useDomains();
  const [showCreate, setShowCreate] = useState(false);

  return (
    <div>
      <PageHeader title="域（资产/演进域）" />
      <p className="text-sm text-gray-500 -mt-3 mb-4">
        域 = 共享仓库 + 验收策略（NL 意图 → 编译检查 → 卡点）。agent 执行的 Goal 必须属于某个域。
      </p>
      <div className="mb-4">
        <Button onClick={() => setShowCreate(true)}>新建域</Button>
      </div>

      {isLoading ? (
        <p className="text-gray-500">加载中…</p>
      ) : !domains?.length ? (
        <Empty>还没有域——创建一个域并挂上你的代码仓库，agentwork 就能在域里全自动干活。</Empty>
      ) : (
        <div className="space-y-4">
          {domains.map((d) => (
            <DomainCard key={d.id} domain={d} />
          ))}
        </div>
      )}

      {showCreate && <CreateDomainDialog onClose={() => setShowCreate(false)} />}
    </div>
  );
}

// DomainCard renders the domain's three lifecycle states (DESIGN.v2.md §5.3):
//   1. not compiled   — NL intent entered, no checks yet → "编译" kicks off the
//                       processor agent.
//   2. compiled, unfrozen — checks exist, checks_compiled_at == '' → the
//                       CONFIRMATION CARD: show what was compiled, freeze or
//                       recompile (the define role stays with the human).
//   3. frozen         — checks_compiled_at set → summary + recompile.
function DomainCard({ domain: initial }: { domain: Domain }) {
  const { data: domain } = useDomain(initial.id);
  const d = domain ?? initial;
  const compile = useCompileDomainPolicy();
  const freeze = useFreezeDomainChecks();
  const { data: agents } = useAgents();
  const [policyText, setPolicyText] = useState(d.policy_text);
  const [processorAgent, setProcessorAgent] = useState("");
  const [compiling, setCompiling] = useState(false);
  const [strength, setStrength] = useState(d.verification_strength);

  const compiled = d.checks_compiled_at !== "";
  const hasChecks = (d.checks.verify?.length ?? 0) > 0 || (d.checks.guards?.length ?? 0) > 0;
  const needsConfirm = !compiled && hasChecks;

  const startCompile = () => {
    if (!policyText.trim() || !processorAgent) return;
    setCompiling(true);
    compile.mutate(
      { id: d.id, policy_text: policyText, processor_agent_id: processorAgent },
      { onSuccess: () => setCompiling(false), onError: () => setCompiling(false) }
    );
  };

  return (
    <div className="rounded-lg border border-gray-200 p-4 space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="font-medium">{d.name}</span>
        <span className={strengthBadge(d.verification_strength)}>{d.verification_strength} 验证</span>
        {compiled ? (
          <span className="inline-flex items-center px-2 py-0.5 text-xs font-medium rounded bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200">策略已冻结</span>
        ) : needsConfirm ? (
          <span className="inline-flex items-center px-2 py-0.5 text-xs font-medium rounded bg-purple-50 text-purple-700 ring-1 ring-purple-200">待确认</span>
        ) : null}
        <span className="text-xs text-gray-500 ml-auto break-all">{d.git_url}</span>
      </div>

      <div className="text-xs text-gray-600 space-y-1">
        <p><span className="font-medium">验收策略（NL）：</span>{d.policy_text || "（未填写——用一句话描述这个域怎么算“干对了”）"}</p>
        {hasChecks && (
          <p>
            <span className="font-medium">编译产物：</span>
            verify=[{d.checks.verify?.join(", ") || "无"}] guards=[{d.checks.guards?.map((g) => `${g.type}(${g.pattern ?? g.min_delta})`).join(", ") || "无"}] gates=[{d.checks.gates?.map((g) => g.name).join(", ") || "无"}]
          </p>
        )}
      </div>

      {!compiled && (
        <div className="space-y-2 border-t border-gray-100 pt-3">
          {needsConfirm ? (
            // Confirmation card: what the processor agent compiled, human decides.
            <div className="rounded bg-amber-50 border border-amber-200 p-3 space-y-2">
              <p className="text-sm font-medium text-amber-900">处理器 agent 已编译验收策略——确认后冻结？</p>
              <pre className="text-xs bg-amber-100 p-2 rounded max-h-48 overflow-auto">{JSON.stringify(d.checks, null, 2)}</pre>
              <div className="flex items-center gap-3 flex-wrap">
                <Field label="验证强度">
                  <select value={strength} onChange={(e) => setStrength(e.target.value)} className={inputCls}>
                    <option value="strong">strong</option>
                    <option value="medium">medium</option>
                    <option value="weak">weak</option>
                  </select>
                </Field>
                <Button
                  disabled={freeze.isPending}
                  onClick={() => freeze.mutate({ id: d.id, checks: d.checks, verification_strength: strength })}
                >
                  {freeze.isPending ? "冻结中…" : "确认并冻结"}
                </Button>
              </div>
            </div>
          ) : (
            <div className="space-y-2">
              <Field label="自然语言验收要求" hint="例如：测试必须通过，改动要带测试，不能动 config/ 下的文件">
                <textarea
                  value={policyText}
                  onChange={(e) => setPolicyText(e.target.value)}
                  className={inputCls}
                  rows={3}
                  placeholder="用一句话描述这个域怎么算“干对了”…"
                />
              </Field>
              <div className="flex items-center gap-3 flex-wrap">
                <Field label="处理器 agent（编译验收策略）">
                  <select value={processorAgent} onChange={(e) => setProcessorAgent(e.target.value)} className={inputCls}>
                    <option value="">选择…</option>
                    {agents?.map((a) => (
                      <option key={a.id} value={a.id}>{a.name}</option>
                    ))}
                  </select>
                </Field>
                <Button onClick={startCompile} disabled={compiling || !policyText.trim() || !processorAgent}>
                  {compiling ? "编译中…" : "编译验收策略"}
                </Button>
              </div>
              {compile.isError && <p className="text-sm text-red-500">{String(compile.error)}</p>}
            </div>
          )}
        </div>
      )}

      {compiled && (
        <div className="flex gap-2 border-t border-gray-100 pt-3">
          <Button variant="outline" onClick={() => startCompile()}>重新编译</Button>
        </div>
      )}
    </div>
  );
}

function strengthBadge(s: string) {
  const base = "inline-flex items-center px-2 py-0.5 text-xs font-medium rounded ring-1";
  if (s === "strong") return `${base} bg-emerald-50 text-emerald-700 ring-emerald-200`;
  if (s === "weak") return `${base} bg-red-50 text-red-700 ring-red-200`;
  return `${base} bg-blue-50 text-blue-700 ring-blue-200`;
}

function CreateDomainDialog({ onClose }: { onClose: () => void }) {
  const create = useCreateDomain();
  const compile = useCompileDomainPolicy();
  const { data: agents } = useAgents();
  const [name, setName] = useState("");
  const [gitUrl, setGitUrl] = useState("");
  const [policyText, setPolicyText] = useState("");
  const [processorAgent, setProcessorAgent] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate(
      { name, git_url: gitUrl, policy_text: policyText, processor_agent_id: processorAgent },
      {
        onSuccess: (d) => {
          if (policyText.trim() && processorAgent) {
            compile.mutate({ id: d.id, policy_text: policyText, processor_agent_id: processorAgent });
          }
          onClose();
        },
      }
    );
  };

  return (
    <Dialog
      title="新建域"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="create-domain-form" disabled={create.isPending || !name.trim() || !gitUrl.trim()}>
            {create.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="create-domain-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} placeholder="如：agentwork" required />
        </Field>
        <Field label="Git 仓库地址" hint="agentwork 会 clone 它作为共享仓库，每个 Goal 一个独立 worktree">
          <input value={gitUrl} onChange={(e) => setGitUrl(e.target.value)} className={inputCls} placeholder="https://github.com/you/repo.git" required />
        </Field>
        <Field label="自然语言验收要求（可选，创建后可再编译）" hint="例如：测试必须通过，改动要带测试">
          <textarea value={policyText} onChange={(e) => setPolicyText(e.target.value)} className={inputCls} rows={3} placeholder="用一句话描述这个域怎么算“干对了”…" />
        </Field>
        <Field label="处理器 agent（可选）">
          <select value={processorAgent} onChange={(e) => setProcessorAgent(e.target.value)} className={inputCls}>
            <option value="">选择…</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        {create.isError && <p className="text-sm text-red-500">{String(create.error)}</p>}
      </form>
    </Dialog>
  );
}
