"use client";

import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  useDomains, useDomain, useAgents, useSquads, useCreateDomain, useUpdateDomain, useCompileDomainPolicy,
  useDomainCompileRun, useFreezeDomainChecks, useGoalEvents, useGateStats, qk,
} from "@/lib/queries";
import { listRunMessages, testDomainGit } from "@/lib/api";
import { useWSEvent } from "@/lib/ws";
import { Button, Dialog, Field, inputCls, PageHeader, Empty, Badge } from "@/components/ui";
import type { Domain, DomainGitTestResult, Checks, Guard, GateRule, Run } from "@/lib/types";

// cutLine truncates one backfilled line for the compile progress box.
function cutLine(t: string): string {
  return t.length > 160 ? t.slice(0, 160) + "…" : t;
}

// GitTestButton probes UNSAVED form values (决策 6-24): repo URL + branch +
// token read permission via `git ls-remote` on the daemon — a misconfigured
// domain used to surface as a failed first run (clone/fetch error) instead.
// onResult reports the outcome to the owning dialog (the create dialog
// gates its submit on it).
function GitTestButton({ gitUrl, defaultBranch, gitCredentials, onResult }: { gitUrl: string; defaultBranch: string; gitCredentials: string; onResult?: (r: DomainGitTestResult) => void }) {
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<DomainGitTestResult | null>(null);
  const run = async () => {
    setTesting(true);
    setResult(null);
    try {
      const r = await testDomainGit({ git_url: gitUrl.trim(), default_branch: defaultBranch.trim(), git_credentials: gitCredentials });
      setResult(r);
      onResult?.(r);
    } catch (e) {
      const r: DomainGitTestResult = { ok: false, branch_exists: false, error: String(e), latency_ms: 0 };
      setResult(r);
      onResult?.(r);
    } finally {
      setTesting(false);
    }
  };
  return (
    <div className="flex items-center gap-2 flex-wrap">
      <Button type="button" variant="outline" onClick={run} disabled={testing || !gitUrl.trim()}>
        {testing ? "测试中…" : "测试连接"}
      </Button>
      {result && (
        <span className={`text-xs ${result.ok && result.branch_exists ? "text-emerald-700" : "text-red-600"}`}>
          {result.ok
            ? (result.refs?.length ?? 0) === 0
              ? "✗ 仓库可达，但没有任何分支（空仓库？）"
              : result.branch_exists
                ? `✓ 仓库可达，分支 ${result.resolved_branch} 存在`
                : `✗ 仓库可达，但分支 ${result.resolved_branch} 不存在（远端: ${result.refs?.join(", ") || "无"}）`
            : `✗ ${result.error}`}
        </span>
      )}
    </div>
  );
}

// renderRunEvent turns one WS run:event (proto.Event shape) into a single
// line for the compile progress stream.
function renderRunEvent(ev: { type?: string; text?: string; tool?: string; input?: string; output?: string }): string {
  const cut = (t: string, n: number) => (t.length > n ? t.slice(0, n) + "…" : t);
  switch (ev.type) {
    case "thought":
      return "💭 " + cut(ev.text ?? "", 160);
    case "message":
      return ev.text ?? "";
    case "tool_use":
      return "🔧 " + (ev.tool ?? "tool") + (ev.input ? " " + cut(ev.input, 80) : "");
    case "tool_result":
      return "· " + cut(ev.output ?? "", 140);
    default:
      return cut(ev.text ?? "", 160);
  }
}

// deriveIssueSrc mirrors the backend's deriveIssueSource: the issue repo +
// platform come from the repo URL — never ask the human twice.
function deriveIssueSrc(gitUrl: string): string {
  let u = gitUrl.trim().replace(/^https?:\/\//, "").replace(/\.git$/, "");
  const at = u.indexOf("@");
  if (at >= 0) u = u.slice(at + 1).replace(":", "/");
  const parts = u.replace(/^\/|\/$/g, "").split("/");
  if (parts.length < 3) return "";
  const host = parts[0];
  const ownerRepo = `${parts[parts.length - 2]}/${parts[parts.length - 1]}`;
  const provider = host.includes("gitcode") ? "GitCode" : host.includes("github") ? "GitHub" : "";
  return provider ? `${ownerRepo} @ ${provider}` : ownerRepo;
}

export default function DomainsPage() {
  useGoalEvents();
  const { data: domains, isLoading } = useDomains();
  const [showCreate, setShowCreate] = useState(false);

  return (
    <div>
      <PageHeader title="项目" />
      <p className="text-sm text-gray-500 -mt-3 mb-4">
        项目 = 共享仓库 + 验收策略（NL 意图 → 编译检查 → 卡点）。agent 执行的 Goal 必须属于某个项目。
      </p>
      <div className="mb-4">
        <Button onClick={() => setShowCreate(true)}>新建项目</Button>
      </div>

      {isLoading ? (
        <p className="text-gray-500">加载中…</p>
      ) : !domains?.length ? (
        <Empty>还没有项目——创建一个项目并挂上你的代码仓库，agentwork 就能在项目里全自动干活。</Empty>
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

// DomainCard renders the domain's three lifecycle states (DESIGN.md §5.3):
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
  // 决策 6-23: 该域最新的 compile processor run——刷新恢复的真相源
  const { data: latestCompile } = useDomainCompileRun(d.id);
  const [showEdit, setShowEdit] = useState(false);
  const [policyText, setPolicyText] = useState(d.policy_text);
  const [compiling, setCompiling] = useState(false);
  const [compileError, setCompileError] = useState<string | null>(null);
  const [compileRun, setCompileRun] = useState<Run | null>(null);
  const [runLines, setRunLines] = useState<string[]>([]);
  // 回显域已配置的处理器 agent（域创建时选的 / 已配过的），不必每次重选
  const [processorAgent, setProcessorAgent] = useState(d.processor_agent_id);
  // 手动编辑验收策略（决策 2-8 降级路径：不依赖模型编译；冻结后也可再编辑）
  const [editChecksOpen, setEditChecksOpen] = useState(false);
  const [dlgSetup, setDlgSetup] = useState<string[]>([]);
  const [dlgExcludes, setDlgExcludes] = useState<string[]>([]);
  const [dlgVerify, setDlgVerify] = useState<string[]>([]);
  const [dlgGuards, setDlgGuards] = useState<Guard[]>([]);
  const [dlgGates, setDlgGates] = useState<GateRule[]>([]);
  const [dlgStrength, setDlgStrength] = useState(d.verification_strength);
  const [dlgError, setDlgError] = useState<string | null>(null);
  const openChecksEditor = () => {
    setDlgSetup([...(d.checks.setup ?? [])]);
    setDlgExcludes([...(d.checks.excludes ?? [])]);
    setDlgVerify([...(d.checks.verify ?? [])]);
    setDlgGuards((d.checks.guards ?? []).map((g) => ({ ...g })));
    setDlgGates((d.checks.gates ?? []).map((g) => ({ ...g })));
    setDlgStrength(d.verification_strength);
    setDlgError(null);
    setEditChecksOpen(true);
  };
  const saveChecks = () => {
    const checks: Checks = {
      ...d.checks,
      setup: dlgSetup,
      excludes: dlgExcludes,
      verify: dlgVerify,
      guards: dlgGuards,
      gates: dlgGates,
    };
    freeze.mutate(
      { id: d.id, checks, verification_strength: dlgStrength },
      {
        onSuccess: () => setEditChecksOpen(false),
        onError: (e) => setDlgError(String(e)),
      }
    );
  };
  const [strength, setStrength] = useState(d.verification_strength);
  // Editable copies of the compiled command lists (the confirmation card is
  // the human's last word: 产物可见、可改、可审 — DESIGN.md §5.3).
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
    setCompileError(null);
    // Compilation is an ASYNC processor run (explores the repo, installs
    // deps, measures the baseline — minutes, not the API round-trip).
    // onSuccess must NOT clear compiling: the API returning only means the
    // run was enqueued. The state clears when the platform's
    // domain:compiled / domain:compile_failed event arrives below.
    compile.mutate(
      { id: d.id, policy_text: policyText, processor_agent_id: processorAgent },
      {
        onSuccess: (run) => {
          setCompileRun(run);
          setRunLines([]);
        },
        onError: () => setCompiling(false),
      }
    );
  };
  // The compile outcome arrives asynchronously as a WS event (the run's
  // terminal state); only then does the "编译中…" state clear and the domain
  // refresh (checks landed / compile failed). Events are global — filter by
  // this domain's id.
  const qc = useQueryClient();
  useWSEvent("domain:compiled", (p) => {
    if ((p as { domain_id?: string })?.domain_id === d.id) {
      setCompiling(false);
      setCompileError(null);
      setCompileRun(null);
      qc.invalidateQueries({ queryKey: qk.domain(d.id) });
    }
  });
  // The compile run's live stream — what the processor agent is doing right
  // now (exploring the repo, installing deps, measuring the baseline).
  useWSEvent("run:event", (p) => {
    const pld = p as { run_id?: string; event?: { type?: string; text?: string; tool?: string; input?: string; output?: string } };
    if (!compileRun || !pld.event || pld.run_id !== compileRun.id) return;
    setRunLines((prev) => [...prev.slice(-40), renderRunEvent(pld.event!)]);
  });
  useWSEvent("domain:compile_failed", (p) => {
    const pld = p as { domain_id?: string; error?: string };
    if (pld?.domain_id === d.id) {
      setCompiling(false);
      setCompileError(pld.error ?? "编译失败（未知原因）");
      setCompileRun(null);
    }
  });
  // 决策 6-23: 刷新恢复。compiling/compileRun 是组件 state，刷新即丢——但
  // 后端持久化了 compile run，mount 时查回最新一条：queued/running → 恢复
  // 横幅（WS 是全局广播、按 run_id 过滤，实时流自动续上）+ content 回填；
  // failed → 恢复失败提示（顺带修"刷新后看不到编译失败"）；completed →
  // 不显示（domain 查询本身带出编译产物/确认卡）。startCompile 在会话内
  // 直接置 state，不动这个查询，互不干扰。
  useEffect(() => {
    if (!latestCompile) return;
    if (latestCompile.status === "queued" || latestCompile.status === "running") {
      setCompiling(true);
      setCompileRun(latestCompile);
      // 回填进度框历史：只取 content（跳过 thought/reasoning 与 tool 行）
      listRunMessages(latestCompile.id)
        .then((msgs) =>
          setRunLines(
            msgs
              .filter((m) => m.role === "assistant" && m.content.trim() !== "")
              .slice(-20)
              .map((m) => cutLine(m.content.trim()))
          )
        )
        .catch(() => {});
    } else if (latestCompile.status === "failed") {
      setCompileError((prev) => prev ?? (latestCompile.result_summary || "编译失败（未知原因）"));
    }
  }, [latestCompile]);

  return (
    <div className="rounded-lg border border-gray-200 p-4 space-y-3">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="font-medium">{d.name}</span>
        {d.type === "scratch" && (
          <span className="inline-flex items-center px-2 py-0.5 text-xs font-medium rounded bg-sky-50 text-sky-700 ring-1 ring-sky-200">无仓库项目</span>
        )}
        <span className={strengthBadge(d.verification_strength)}>{d.verification_strength} 验证</span>
        {compiled ? (
          <span className="inline-flex items-center px-2 py-0.5 text-xs font-medium rounded bg-emerald-50 text-emerald-700 ring-1 ring-emerald-200">策略已冻结</span>
        ) : needsConfirm ? (
          <span className="inline-flex items-center px-2 py-0.5 text-xs font-medium rounded bg-purple-50 text-purple-700 ring-1 ring-purple-200">待确认</span>
        ) : null}
        {d.type !== "scratch" && <span className="text-xs text-gray-500 ml-auto break-all">{d.git_url}</span>}
        {d.type === "scratch" && d.scratch_dir && (
          <span className="text-xs text-gray-500 ml-auto break-all font-mono">{d.scratch_dir}</span>
        )}
        <button
          onClick={() => setShowEdit(true)}
          className="text-xs text-indigo-600 hover:text-indigo-800 hover:underline shrink-0"
        >
          编辑
        </button>
      </div>

      {showEdit && <EditDomainDialog domain={d} onClose={() => setShowEdit(false)} />}

      <div className="text-xs text-gray-600 space-y-1">
        <p><span className="font-medium">验收策略（NL）：</span>{d.policy_text || "（未填写——用一句话描述这个项目怎么算“干对了”）"}</p>
        {d.metrics_baseline && d.metrics_baseline !== "{}" && d.metrics_baseline !== "" && (
          <p>
            <span className="font-medium">演进基线（决策 2-15）：</span>
            {(() => {
              try {
                const m = JSON.parse(d.metrics_baseline);
                return `测试 ${m.test_count ?? "?"} 个 · 覆盖率 ${m.coverage ?? "?"}%`;
              } catch {
                return d.metrics_baseline;
              }
            })()}
          </p>
        )}
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
                {d.type !== "scratch" && (
                  <Field label="excludes（提交时排除的路径，每行一条）" hint="依赖目录，如 **/node_modules/**">
                    <textarea
                      value={editExcludes ?? (d.checks.excludes ?? []).join("\n")}
                      onChange={(e) => setEditExcludes(e.target.value)}
                      className={inputCls}
                      rows={4}
                      placeholder={"**/node_modules/**"}
                    />
                  </Field>
                )}
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
                  placeholder="用一句话描述这个项目怎么算“干对了”…"
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
                <Button variant="outline" onClick={openChecksEditor}>
                  手动编辑
                </Button>
              </div>
            </div>
          )}
        </div>
      )}

      {compiled && (
        <div className="flex gap-2 border-t border-gray-100 pt-3">
          <Button variant="outline" onClick={() => startCompile()} disabled={compiling}>
            {compiling ? "编译中…" : "重新编译"}
          </Button>
          <Button variant="outline" onClick={openChecksEditor}>
            编辑策略
          </Button>
        </div>
      )}

      {/* 编译进行中反馈——两种状态（首次编译 / 重新编译）共享：编译是异步
          processor run，compiling 保持到 domain:compiled / compile_failed
          事件到达（API 返回只是 run 入队，编译本身要几分钟）。compileRun
          打开实时进度流——能看到处理器 agent 此刻在干什么。刷新后由
          useDomainCompileRun 恢复（决策 6-23）。 */}
      {compiling && compileRun && (
        <div className="border-t border-gray-100 pt-2">
          <div className="flex items-center gap-2 text-xs text-amber-700 mb-1.5" title={compileRun.id}>
            <span className="inline-block w-2 h-2 rounded-full bg-amber-500 animate-pulse" />
            {agents?.find((a) => a.id === compileRun.agent_id)?.name ?? compileRun.agent_id.slice(0, 8)} 正在探索项目、统计基线，请稍等......
          </div>
          <div className="rounded bg-zinc-50 border border-zinc-200 p-2 max-h-44 overflow-y-auto space-y-0.5 font-mono text-[11px] text-zinc-600">
            {runLines.length === 0 ? (
              <span className="text-zinc-400">等待 agent 开始…</span>
            ) : (
              runLines.map((l, i) => (
                <div key={i} className="whitespace-pre-wrap break-words">{l}</div>
              ))
            )}
          </div>
        </div>
      )}
      {compile.isError && <p className="text-sm text-red-500">{String(compile.error)}</p>}
      {compileError && <p className="text-sm text-red-500">编译失败：{compileError}</p>}

      {/* 手动编辑验收策略（决策 2-8）：不依赖模型编译，直接写 checks 后冻结；
          冻结后也可再编辑（重新冻结更新 checks_compiled_at）。 */}
      {editChecksOpen && (
        <Dialog
          title="编辑验收策略"
          onClose={() => setEditChecksOpen(false)}
          footer={
            <div className="flex gap-2 justify-end">
              <Button variant="ghost" onClick={() => setEditChecksOpen(false)}>取消</Button>
              <Button onClick={saveChecks} disabled={freeze.isPending}>
                {freeze.isPending ? "保存中…" : "保存并冻结"}
              </Button>
            </div>
          }
        >
          <div className="space-y-3">
            <Field label="环境准备 setup（幂等命令）">
              <div className="space-y-2">
                {dlgSetup.map((v, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <input
                      value={v}
                      onChange={(e) => setDlgSetup(dlgSetup.map((x, j) => j === i ? e.target.value : x))}
                      className={inputCls}
                      placeholder="cd web && npm install"
                    />
                    <button onClick={() => setDlgSetup(dlgSetup.filter((_, j) => j !== i))} className="text-red-500 shrink-0 px-1" title="删除这条">✕</button>
                  </div>
                ))}
                <button onClick={() => setDlgSetup([...dlgSetup, ""])} className="text-xs text-indigo-600 hover:underline">+ 添加命令</button>
              </div>
            </Field>
            {d.type !== "scratch" && (
            <Field label="提交排除 excludes（glob）">
              <div className="space-y-2">
                {dlgExcludes.map((v, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <input
                      value={v}
                      onChange={(e) => setDlgExcludes(dlgExcludes.map((x, j) => j === i ? e.target.value : x))}
                      className={inputCls}
                      placeholder="**/node_modules/**"
                    />
                    <button onClick={() => setDlgExcludes(dlgExcludes.filter((_, j) => j !== i))} className="text-red-500 shrink-0 px-1" title="删除这条">✕</button>
                  </div>
                ))}
                <button onClick={() => setDlgExcludes([...dlgExcludes, ""])} className="text-xs text-indigo-600 hover:underline">+ 添加排除</button>
              </div>
            </Field>
            )}
            <Field label="机器验证 verify（exit 0 为过）">
              <div className="space-y-2">
                {dlgVerify.map((v, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <input
                      value={v}
                      onChange={(e) => setDlgVerify(dlgVerify.map((x, j) => j === i ? e.target.value : x))}
                      className={inputCls}
                      placeholder="go test ./..."
                    />
                    <button onClick={() => setDlgVerify(dlgVerify.filter((_, j) => j !== i))} className="text-red-500 shrink-0 px-1" title="删除这条">✕</button>
                  </div>
                ))}
                <button onClick={() => setDlgVerify([...dlgVerify, ""])} className="text-xs text-indigo-600 hover:underline">+ 添加命令</button>
              </div>
            </Field>
            {d.type !== "scratch" && (
            <Field label="结构化约束 guards（机器检查的硬约束）">
              <div className="space-y-2">
                {dlgGuards.map((g, i) => (
                  <div key={i} className="flex items-start gap-2">
                    <div className="flex-1 space-y-1.5 rounded-lg border border-gray-200 p-2">
                      <select
                        value={g.type}
                        onChange={(e) => setDlgGuards(dlgGuards.map((x, j) => j === i ? { ...x, type: e.target.value } : x))}
                        className={inputCls}
                      >
                        <option value="diff_contains">diff_contains（改动必含）</option>
                        <option value="diff_excludes">diff_excludes（改动禁触）</option>
                        <option value="coverage_delta">coverage_delta（覆盖率提升）</option>
                      </select>
                    {g.type !== "coverage_delta" && (
                      <input
                        value={g.pattern}
                        onChange={(e) => setDlgGuards(dlgGuards.map((x, j) => j === i ? { ...x, pattern: e.target.value } : x))}
                        className={inputCls}
                        placeholder={g.type === "diff_contains" ? "改动必须包含的 glob，如 **/*_test.go" : "禁止触碰的 glob，如 config/*"}
                      />
                    )}
                    {g.type === "coverage_delta" && (
                      <input
                        type="number"
                        value={g.min_delta}
                        onChange={(e) => setDlgGuards(dlgGuards.map((x, j) => j === i ? { ...x, min_delta: Number(e.target.value) } : x))}
                        className={inputCls}
                        placeholder="覆盖率提升百分点"
                      />
                    )}
                    </div>
                    <button
                      onClick={() => setDlgGuards(dlgGuards.filter((_, j) => j !== i))}
                      className="text-red-500 shrink-0 mt-2 px-1"
                      title="删除这条约束"
                    >✕</button>
                  </div>
                ))}
                <button
                  onClick={() => setDlgGuards([...dlgGuards, { type: "diff_contains", pattern: "", min_delta: 0 }])}
                  className="text-xs text-indigo-600 hover:underline"
                >
                  + 添加约束
                </button>
              </div>
            </Field>
            )}
            {d.type !== "scratch" && (
            <Field label="卡点规则 gates（何时必须停给人审批）">
              <div className="space-y-2">
                {dlgGates.map((g, i) => (
                  <div key={i} className="flex items-start gap-2">
                    <div className="flex-1 space-y-1.5 rounded-lg border border-gray-200 p-2">
                      <select
                        value={g.name}
                        onChange={(e) => setDlgGates(dlgGates.map((x, j) => j === i ? { ...x, name: e.target.value } : x))}
                        className={inputCls}
                      >
                        <option value="merge">merge（每次完成必审）</option>
                        <option value="diff_contains">diff_contains（改动命中必审）</option>
                        <option value="diff_excludes">diff_excludes（触碰禁路必审）</option>
                      </select>
                    <input
                      value={g.when}
                      onChange={(e) => setDlgGates(dlgGates.map((x, j) => j === i ? { ...x, when: e.target.value } : x))}
                      className={inputCls}
                      placeholder="审批时给人看的触发说明（如：测试通过且覆盖率不低于基线，人工确认测试充分性）"
                    />
                    {g.name !== "merge" && (
                      <input
                        value={g.pattern}
                        onChange={(e) => setDlgGates(dlgGates.map((x, j) => j === i ? { ...x, pattern: e.target.value } : x))}
                        className={inputCls}
                        placeholder="机器判定的 glob（如 config/*）"
                      />
                    )}
                    </div>
                    <button
                      onClick={() => setDlgGates(dlgGates.filter((_, j) => j !== i))}
                      className="text-red-500 shrink-0 mt-2 px-1"
                      title="删除这条卡点"
                    >✕</button>
                  </div>
                ))}
                <button
                  onClick={() => setDlgGates([...dlgGates, { name: "merge", when: "", pattern: "" }])}
                  className="text-xs text-indigo-600 hover:underline"
                >
                  + 添加卡点
                </button>
              </div>
            </Field>
            )}
            <Field label="验证强度">
              <select value={dlgStrength} onChange={(e) => setDlgStrength(e.target.value)} className={inputCls}>
                <option value="strong">strong（强验证，默认少卡点）</option>
                <option value="medium">medium</option>
                <option value="weak">weak（弱验证，强制人工段）</option>
              </select>
            </Field>
            {dlgError && <p className="text-sm text-red-500">{dlgError}</p>}
          </div>
        </Dialog>
      )}
    </div>
  );
}

// GateHealthTable shows each gate rule's decision history (M2 health data,
// DESIGN.md §13): a gate approved every time is a candidate for removal;
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

// EditDomainDialog edits a domain's mutable configuration — the issue
// handler can be switched (agent → squad) after creation, and the repo /
// credentials updated. Compile artifacts stay untouched (they have their
// own freeze/recompile flow).
function EditDomainDialog({ domain, onClose }: { domain: Domain; onClose: () => void }) {
  const update = useUpdateDomain();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const [gitUrl, setGitUrl] = useState(domain.git_url);
  const [defaultBranch, setDefaultBranch] = useState(domain.default_branch || "main");
  const [gitIdentity, setGitIdentity] = useState(domain.git_identity);
  const [gitCredentials, setGitCredentials] = useState(domain.git_credentials);
  const [issueRepo, setIssueRepo] = useState(domain.issue_repo);
  const [issueAssigneeType, setIssueAssigneeType] = useState(domain.issue_assignee_type || "agent");
  const [issueAssignee, setIssueAssignee] = useState(domain.issue_assignee);
  const [issueProvider, setIssueProvider] = useState(domain.issue_provider || "github");
  // 决策 6-24 延伸：编辑不能绕过创建门槛——git 配置动过就必须重测通过
  // 才能保存（后端同样强制）；没动 git 配置时不拦（仓库已失效仍可改
  // issue 处理方等无关字段）。git 字段一改，旧结果失效。
  const [gitTestPassed, setGitTestPassed] = useState<boolean | null>(null);
  const gitDirty =
    gitUrl !== domain.git_url ||
    defaultBranch !== (domain.default_branch || "main") ||
    gitCredentials !== domain.git_credentials;
  const gitGateBlocks = gitDirty && gitTestPassed !== true;

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    update.mutate(
      {
        id: domain.id,
        body: {
          git_url: gitUrl,
          default_branch: defaultBranch,
          git_identity: gitIdentity,
          git_credentials: gitCredentials,
          issue_repo: issueRepo,
          issue_assignee: issueAssignee,
          issue_assignee_type: issueAssigneeType,
          issue_provider: issueProvider,
        },
      },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title={`编辑项目：${domain.name}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="edit-domain-form" disabled={update.isPending || !gitUrl.trim() || gitGateBlocks}>
            {update.isPending ? "保存中…" : "保存"}
          </Button>
        </>
      }
    >
      <form id="edit-domain-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="Git 仓库地址">
          <input value={gitUrl} onChange={(e) => { setGitUrl(e.target.value); setGitTestPassed(null); }} className={inputCls} required />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label="默认分支">
            <input value={defaultBranch} onChange={(e) => { setDefaultBranch(e.target.value); setGitTestPassed(null); }} className={inputCls} />
          </Field>
          <Field label="Git 身份（commit 作者）">
            <input value={gitIdentity} onChange={(e) => setGitIdentity(e.target.value)} className={inputCls} placeholder="agentwork[bot] <bot@local>" />
          </Field>
        </div>
        <Field label="平台令牌（git_credentials，远程操作身份）">
          <input value={gitCredentials} onChange={(e) => { setGitCredentials(e.target.value); setGitTestPassed(null); }} className={inputCls} type="password" placeholder="bot 账号的 token" />
        </Field>
        <GitTestButton gitUrl={gitUrl} defaultBranch={defaultBranch} gitCredentials={gitCredentials} onResult={(r) => setGitTestPassed(r.ok && r.branch_exists)} />
        {gitGateBlocks && (
          <p className="text-xs text-amber-600">⚠ git 配置已改动，需重新通过「测试连接」才能保存</p>
        )}
        <Field label="Issue 追踪（M4-B）" hint="open issue 自动变成任务，处理完自动 close">
          <input value={issueRepo} onChange={(e) => setIssueRepo(e.target.value)} className={inputCls} placeholder="owner/repo" />
          <div className="mt-2 flex gap-2 items-center">
            <label className="text-xs text-gray-500">平台：</label>
            <select value={issueProvider} onChange={(e) => setIssueProvider(e.target.value)} className={inputCls}>
              <option value="github">GitHub</option>
              <option value="gitcode">GitCode</option>
            </select>
          </div>
        </Field>
        <Field label="issue 处理方" hint="agent 或 squad——处理方随时可以换">
          <div className="flex gap-2 items-center">
            <select
              value={issueAssigneeType}
              onChange={(e) => { setIssueAssigneeType(e.target.value); setIssueAssignee(""); }}
              className={inputCls}
            >
              <option value="agent">Agent</option>
              <option value="squad">Squad</option>
            </select>
            <select value={issueAssignee} onChange={(e) => setIssueAssignee(e.target.value)} className={inputCls}>
              <option value="">{issueAssigneeType === "agent" ? "选择 agent…" : "选择 squad…"}</option>
              {issueAssigneeType === "agent"
                ? agents?.map((a) => (
                    <option key={a.id} value={a.id}>{a.name}</option>
                  ))
                : squads?.map((s) => (
                    <option key={s.id} value={s.id}>{s.name}</option>
                  ))}
            </select>
          </div>
        </Field>
        {update.isError && <p className="text-sm text-red-500">{String(update.error)}</p>}
      </form>
    </Dialog>
  );
}

function CreateDomainDialog({ onClose }: { onClose: () => void }) {
  const create = useCreateDomain();
  const compile = useCompileDomainPolicy();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const [name, setName] = useState("");
  const [domainType, setDomainType] = useState<"repo" | "scratch">("repo");
  const [gitUrl, setGitUrl] = useState("");
  const [defaultBranch, setDefaultBranch] = useState("");
  const [policyText, setPolicyText] = useState("");
  const [processorAgent, setProcessorAgent] = useState("");
  const [issueAssigneeType, setIssueAssigneeType] = useState("agent");
  const [issueAssignee, setIssueAssignee] = useState("");
  const [gitCredentials, setGitCredentials] = useState("");
  const [gitIdentity, setGitIdentity] = useState("");
  // 决策 6-24 延伸：repo 域必须通过「测试连接」才能创建（后端同样强制，
  // 这里是前端门槛）。git 字段一改，之前的测试结果即失效——必须重测。
  const [gitTestPassed, setGitTestPassed] = useState<boolean | null>(null);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate(
      { name, type: domainType, git_url: domainType === "scratch" ? "" : gitUrl, default_branch: domainType === "scratch" ? "" : defaultBranch, policy_text: policyText, processor_agent_id: processorAgent, issue_repo: "", issue_assignee: issueAssignee, issue_assignee_type: issueAssigneeType, issue_provider: "", git_credentials: gitCredentials, git_identity: gitIdentity },
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
      title="新建项目"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="create-domain-form" disabled={create.isPending || !name.trim() || (domainType === "repo" && (!gitUrl.trim() || gitTestPassed !== true))}>
            {create.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="create-domain-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} placeholder="如：agentwork" required />
        </Field>
        <Field label="项目类型">
          <div className="flex gap-2">
            <label className={`flex-1 flex items-center gap-2 rounded-lg border px-3 py-2 cursor-pointer text-sm ${domainType === "repo" ? "border-indigo-400 bg-indigo-50 text-indigo-700" : "border-zinc-200 text-zinc-600"}`}>
              <input type="radio" className="accent-indigo-500" checked={domainType === "repo"} onChange={() => setDomainType("repo")} />
              代码仓库
            </label>
            <label className={`flex-1 flex items-center gap-2 rounded-lg border px-3 py-2 cursor-pointer text-sm ${domainType === "scratch" ? "border-indigo-400 bg-indigo-50 text-indigo-700" : "border-zinc-200 text-zinc-600"}`}>
              <input type="radio" className="accent-indigo-500" checked={domainType === "scratch"} onChange={() => setDomainType("scratch")} />
              无仓库项目
            </label>
          </div>
          {domainType === "scratch" && (
            <p className="mt-1.5 text-xs text-zinc-500">研究/信息类任务：持久项目目录，产出是汇报，人卡点强制</p>
          )}
        </Field>
        {domainType === "repo" && (
        <Field label="Git 仓库地址" hint="agentwork 会 clone 它作为共享仓库，每个 Goal 一个独立 worktree">
          <input value={gitUrl} onChange={(e) => { setGitUrl(e.target.value); setGitTestPassed(null); }} className={inputCls} placeholder="https://github.com/you/repo.git" required />
        </Field>
        )}
        {domainType === "repo" && (
        <Field label="默认分支" hint="留空 = 仓库的默认分支（main/master 自动探测）">
          <input value={defaultBranch} onChange={(e) => { setDefaultBranch(e.target.value); setGitTestPassed(null); }} className={inputCls} placeholder="main" />
        </Field>
        )}
        <details className="text-xs">
          <summary className="cursor-pointer text-zinc-400 hover:text-zinc-600">高级设置</summary>
          <div className="mt-2 space-y-4">
            {domainType === "repo" && (
              <Field label="Git 身份（commit 作者，可选）" hint='格式：名字 &lt;邮箱&gt;，如 agentwork[bot] &lt;bot@local&gt;——不填默认 agentwork[bot]'>
                <input value={gitIdentity} onChange={(e) => setGitIdentity(e.target.value)} className={inputCls} placeholder="agentwork[bot] <bot@local>" />
              </Field>
            )}
            <Field label="自然语言验收要求（可选，创建后可再编译）" hint="例如：测试必须通过，改动要带测试">
              <textarea value={policyText} onChange={(e) => setPolicyText(e.target.value)} className={inputCls} rows={3} placeholder="用一句话描述这个项目怎么算“干对了”…" />
            </Field>
            <Field label="处理器 agent（可选）">
              <select value={processorAgent} onChange={(e) => setProcessorAgent(e.target.value)} className={inputCls}>
                <option value="">选择…</option>
                {agents?.map((a) => (
                  <option key={a.id} value={a.id}>{a.name}</option>
                ))}
              </select>
            </Field>
          </div>
        </details>
        {domainType === "repo" && (
        <>
        <Field label="issue 处理方（选填）" hint={`issue 仓库与平台从仓库地址自动识别${deriveIssueSrc(gitUrl) ? `：${deriveIssueSrc(gitUrl)}` : ""}；选了处理方后，仓库的 open issue 自动变成任务，处理完自动 close`}>
          <div className="flex gap-2 items-center">
            <select
              value={issueAssigneeType}
              onChange={(e) => { setIssueAssigneeType(e.target.value); setIssueAssignee(""); }}
              className={inputCls}
            >
              <option value="agent">Agent</option>
              <option value="squad">Squad</option>
            </select>
            <select value={issueAssignee} onChange={(e) => setIssueAssignee(e.target.value)} className={inputCls}>
              <option value="">选择…</option>
              {issueAssigneeType === "agent"
                ? agents?.map((a) => (
                    <option key={a.id} value={a.id}>{a.name}</option>
                  ))
                : squads?.map((s) => (
                    <option key={s.id} value={s.id}>{s.name}</option>
                  ))}
            </select>
          </div>
        </Field>
        <Field label="平台操作 token（git_credentials）" hint="bot 账号 token（决策 3-5）：issue 评论/close + git push 都以此身份出现。权限需覆盖：仓库读写（Contents）+ issues 读写。GitHub 用 fine-grained PAT 只授权本仓库；GitCode 用 token-classic">
          <input value={gitCredentials} onChange={(e) => { setGitCredentials(e.target.value); setGitTestPassed(null); }} className={inputCls} placeholder="GitHub PAT 或 GitCode token（bot 账号，需仓库+issue 读写）" type="password" />
        </Field>
        <GitTestButton gitUrl={gitUrl} defaultBranch={defaultBranch} gitCredentials={gitCredentials} onResult={(r) => setGitTestPassed(r.ok && r.branch_exists)} />
        {gitTestPassed !== true && (
          <p className="text-xs text-amber-600">⚠ 需先通过「测试连接」才能创建（后端同样强制）</p>
        )}
        </>
        )}
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
