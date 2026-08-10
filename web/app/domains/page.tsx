"use client";

import { useMemo, useState } from "react";
import {
  useDomains, useDomain, useAgents, useCreateDomain, useCompileDomainPolicy,
  useFreezeDomainChecks, useGoalEvents, useGateStats,
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

      <GateHealthTable />

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
  // Editable copies of the compiled command lists (the confirmation card is
  // the human's last word: 产物可见、可改、可审 — DESIGN.v2.md §5.3).
  // jsonDraft overrides everything when edited (guards/gates included).
  const [editSetup, setEditSetup] = useState<string | null>(null);
  const [editExcludes, setEditExcludes] = useState<string | null>(null);
  const [editVerify, setEditVerify] = useState<string | null>(null);
  const [jsonDraft, setJsonDraft] = useState<string | null>(null);
  const [jsonError, setJsonError] = useState<string | null>(null);

  const compiled = d.checks_compiled_at !== "";
  const hasChecks = (d.checks.verify?.length ?? 0) > 0 || (d.checks.guards?.length ?? 0) > 0;
  const needsConfirm = !compiled && hasChecks;

  // The checks the human would freeze: the JSON draft wins when edited (it
  // covers guards/gates too), else the field editors win, else compiled.
  const editableChecks: Checks = useMemo(() => {
    if (jsonDraft !== null) {
      try {
        return JSON.parse(jsonDraft) as Checks;
      } catch {
        return d.checks; // invalid draft — freeze stays on compiled
      }
    }
    return {
      ...d.checks,
      setup: splitLines(editSetup ?? (d.checks.setup ?? []).join("\n")),
      excludes: splitLines(editExcludes ?? (d.checks.excludes ?? []).join("\n")),
      verify: splitLines(editVerify ?? (d.checks.verify ?? []).join("\n")),
    };
  }, [jsonDraft, editSetup, editExcludes, editVerify, d.checks]);

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
            // Confirmation card: what the processor agent compiled, human
            // decides — and can edit (产物可见、可改、可审).
            <div className="rounded bg-amber-50 border border-amber-200 p-3 space-y-2">
              <p className="text-sm font-medium text-amber-900">处理器 agent 已编译验收策略——检查后可编辑，确认后冻结？</p>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
                <Field label="setup（验证前环境准备，幂等，每行一条）" hint="依赖安装，如 cd web && npm install">
                  <textarea
                    value={editSetup ?? (d.checks.setup ?? []).join("\n")}
                    onChange={(e) => setEditSetup(e.target.value)}
                    className={inputCls}
                    rows={4}
                    placeholder={"cd web && npm install"}
                  />
                </Field>
                <Field label="excludes（提交时排除的路径，每行一条）" hint="依赖目录，如 **/node_modules/**">
                  <textarea
                    value={editExcludes ?? (d.checks.excludes ?? []).join("\n")}
                    onChange={(e) => setEditExcludes(e.target.value)}
                    className={inputCls}
                    rows={4}
                    placeholder={"**/node_modules/**"}
                  />
                </Field>
                <Field label="verify（机器验证命令，每行一条）" hint="exit 0 = 通过">
                  <textarea
                    value={editVerify ?? (d.checks.verify ?? []).join("\n")}
                    onChange={(e) => setEditVerify(e.target.value)}
                    className={inputCls}
                    rows={4}
                    placeholder={"go test ./..."}
                  />
                </Field>
              </div>
              <div className="text-xs text-gray-600 space-y-0.5">
                <p><span className="font-medium">guards：</span>{d.checks.guards?.length ? d.checks.guards.map((g) => `${g.type}(${g.pattern ?? g.min_delta})`).join(", ") : "无"}</p>
                <p><span className="font-medium">gates：</span>{d.checks.gates?.length ? d.checks.gates.map((g) => g.name).join(", ") : "无"}</p>
                <details className="pt-1">
                  <summary className="cursor-pointer text-amber-800 font-medium">编辑完整 JSON（setup/verify/guards/gates）</summary>
                  <textarea
                    value={jsonDraft ?? JSON.stringify(d.checks, null, 2)}
                    onChange={(e) => {
                      setJsonDraft(e.target.value);
                      setJsonError(null);
                      try {
                        JSON.parse(e.target.value);
                      } catch {
                        setJsonError("JSON 格式错误——冻结将使用编译产物");
                      }
                    }}
                    className="mt-1 w-full rounded border border-amber-300 bg-amber-100 px-2 py-1.5 text-xs font-mono"
                    rows={10}
                  />
                  {jsonError && <p className="text-xs text-red-600">{jsonError}</p>}
                </details>
              </div>
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
                  onClick={() => freeze.mutate({ id: d.id, checks: editableChecks, verification_strength: strength })}
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

// GateHealthTable shows each gate rule's decision history (M2 health data,
// DESIGN.v2.md §13): a gate approved every time is a candidate for removal;
// one rejected repeatedly for tightening.
function GateHealthTable() {
  const { data: stats } = useGateStats();
  if (!stats?.length) return null;
  return (
    <div className="mt-8">
      <h3 className="text-sm font-medium mb-2">卡点健康度</h3>
      <div className="rounded-lg border border-gray-200 overflow-hidden">
        <table className="w-full text-sm">
          <thead className="bg-gray-50 text-left text-xs text-gray-500">
            <tr>
              <th className="px-3 py-2">卡点规则</th>
              <th className="px-3 py-2">决策总数</th>
              <th className="px-3 py-2">批准</th>
              <th className="px-3 py-2">驳回</th>
              <th className="px-3 py-2">建议</th>
            </tr>
          </thead>
          <tbody>
            {stats.map((s) => {
              const rate = s.total > 0 ? s.approved / s.total : 0;
              const advice =
                s.total >= 5 && rate >= 0.95
                  ? "连批率高——建议放宽或移除"
                  : s.total >= 3 && s.rejected >= 3
                    ? "频繁驳回——建议收紧规则或改自动"
                    : "—";
              return (
                <tr key={s.rule} className="border-t border-gray-100">
                  <td className="px-3 py-2 font-mono text-xs">{s.rule}</td>
                  <td className="px-3 py-2">{s.total}</td>
                  <td className="px-3 py-2 text-emerald-700">{s.approved}</td>
                  <td className="px-3 py-2 text-red-700">{s.rejected}</td>
                  <td className="px-3 py-2 text-xs text-gray-600">{advice}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
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
  const [issueRepo, setIssueRepo] = useState("");
  const [issueAssignee, setIssueAssignee] = useState("");
  const [issueProvider, setIssueProvider] = useState("github");
  const [gitCredentials, setGitCredentials] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate(
      { name, git_url: gitUrl, policy_text: policyText, processor_agent_id: processorAgent, issue_repo: issueRepo, issue_assignee: issueAssignee, issue_provider: issueProvider, git_credentials: gitCredentials },
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
        <Field label="Issue 追踪（可选，M4-B）" hint="仓库的 open issue 自动变成任务，处理完自动 close">
          <input value={issueRepo} onChange={(e) => setIssueRepo(e.target.value)} className={inputCls} placeholder="owner/repo，如 yusheng-g/agentwork" />
          <div className="mt-2 flex gap-2 items-center">
            <label className="text-xs text-gray-500">平台：</label>
            <select value={issueProvider} onChange={(e) => setIssueProvider(e.target.value)} className={inputCls}>
              <option value="github">GitHub</option>
              <option value="gitcode">GitCode</option>
            </select>
          </div>
        </Field>
        <Field label="issue 处理 agent（选填，配了 issue_repo 后生效）">
          <select value={issueAssignee} onChange={(e) => setIssueAssignee(e.target.value)} className={inputCls}>
            <option value="">选择…</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        <Field label="GitHub token（git_credentials，issue 追踪用）">
          <input value={gitCredentials} onChange={(e) => setGitCredentials(e.target.value)} className={inputCls} placeholder="ghp_… 或 fine-grained PAT" type="password" />
        </Field>
        {create.isError && <p className="text-sm text-red-500">{String(create.error)}</p>}
      </form>
    </Dialog>
  );
}

// splitLines turns an editor textarea's content into a command list: one
// command per line, blank lines dropped.
function splitLines(s: string): string[] {
  return s
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.length > 0);
}
