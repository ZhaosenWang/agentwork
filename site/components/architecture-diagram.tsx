"use client";

import { useTranslations } from "next-intl";
import { ArrowDown, ArrowRight } from "lucide-react";

// 真正的视觉架构图（不是 ASCII 搬运）——三层流向：
//   Web UI → Daemon（含 SQLite/bus/scheduler 三个内部块）→ agentwork connect → Agent CLI
// 层间用绿色箭头标注协议/方法（/backend 代理、/connect JSON-RPC、run.poll、runtime.Open）。
// 深色卡片 + 终端绿流向线，和整站调性一致。
//
// 准确性依据（核对 internal/server、internal/daemon、internal/link、web/next.config.ts）：
//   - web 通过 /backend/* + /ws 同源代理打 daemon（next.config.ts rewrites）
//   - daemon: HTTP API + WS hub, SQLite 是真相源, bus 是唤醒提示, PULL dispatch (run.poll)
//   - daemon↔machine: /connect 上 JSON-RPC over WS
//   - machine: runtime.Open (acp/jsonl/jsonrpc over stdio/ws/tcp) spawn agent CLI
//
// namespace 默认 "landing.arch"；docs 页传 "landing.arch" 复用同一图
// （图是共享资产，docs 页额外有自己的 layers/stack 详情）。
export function ArchitectureDiagram({
  namespace = "landing.arch",
}: {
  namespace?: string;
}) {
  const t = useTranslations(namespace);

  return (
    <div className="rounded-xl border border-border bg-surface p-6 font-mono">
      <Layer title={t("web.title")} sub={t("web.sub")} note={t("web.note")} />

      <Flow label={t("proxy")} />

      <Layer title={t("daemon.title")} sub={t("daemon.sub")} note={t("daemon.note")}>
        <div className="mt-3 grid grid-cols-3 gap-2 text-[11px]">
          <InnerBlock label={t("sqlite")} />
          <InnerBlock label={t("bus")} />
          <InnerBlock label={t("scheduler")} />
        </div>
      </Layer>

      <Flow label={t("link")} />
      <Flow label={t("pull")} compact />

      <Layer title={t("machine.title")} sub={t("machine.sub")} note={t("machine.note")} />

      <Flow label={t("runtime")} />

      <Layer title={t("cli.title")} sub={t("cli.sub")} note={t("cli.note")} last />

      <div className="mt-4 flex items-center gap-2 border-t border-border pt-4 text-[11px] text-muted">
        <ArrowRight size={13} className="text-accent" />
        {t("deliver")}
      </div>
    </div>
  );
}

function Layer({
  title,
  sub,
  note,
  last,
  children,
}: {
  title: string;
  sub: string;
  note: string;
  last?: boolean;
  children?: React.ReactNode;
}) {
  return (
    <div
      className={
        last
          ? "rounded-lg border border-border bg-bg p-4"
          : "rounded-lg border border-accent/30 bg-bg p-4"
      }
    >
      <div className="flex items-baseline gap-2">
        <span className="text-sm font-semibold text-accent">{title}</span>
        <span className="text-[11px] text-muted">{sub}</span>
      </div>
      <div className="mt-0.5 text-[11px] text-muted">{note}</div>
      {children}
    </div>
  );
}

function InnerBlock({ label }: { label: string }) {
  return (
    <div className="rounded border border-border bg-surface-2 px-2 py-1.5 text-center text-muted">
      {label}
    </div>
  );
}

function Flow({ label, compact }: { label: string; compact?: boolean }) {
  return (
    <div className={`flex items-center justify-center gap-2 ${compact ? "py-0.5" : "py-2"}`}>
      <ArrowDown size={14} className="text-accent" />
      <span className="rounded-full border border-border bg-surface-2 px-2 py-0.5 text-[10px] text-muted">
        {label}
      </span>
    </div>
  );
}
