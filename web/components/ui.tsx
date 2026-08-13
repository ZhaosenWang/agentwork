"use client";

import { useEffect, useRef } from "react";
import { cn } from "@/lib/utils";

// ── Button ──
type ButtonVariant = "primary" | "ghost" | "danger" | "outline";

const BTN_VARIANTS: Record<ButtonVariant, string> = {
  primary:
    "bg-gradient-to-b from-indigo-600 to-violet-600 text-white shadow-sm shadow-indigo-500/25 hover:shadow-md hover:shadow-indigo-500/30 hover:-translate-y-px",
  outline:
    "border border-zinc-300 bg-white text-zinc-700 hover:border-indigo-300 hover:text-indigo-700 hover:bg-indigo-50/50",
  ghost: "text-zinc-600 hover:bg-indigo-50/60 hover:text-indigo-700",
  danger:
    "bg-gradient-to-b from-red-500 to-rose-600 text-white shadow-sm shadow-red-500/25 hover:shadow-md hover:shadow-red-500/30 hover:-translate-y-px",
};

export function Button({
  variant = "primary",
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { variant?: ButtonVariant }) {
  return (
    <button
      className={cn(
        "inline-flex items-center justify-center px-3 py-1.5 text-sm font-medium rounded-full transition-all duration-150 disabled:opacity-50 disabled:cursor-not-allowed disabled:hover:translate-y-0 disabled:hover:shadow-none",
        BTN_VARIANTS[variant],
        className
      )}
      {...props}
    />
  );
}

// ── Badge ──
// pill 形 + 状态圆点：同一状态一个主色，圆点给"活着"的提示。
const STATUS_COLORS: Record<string, string> = {
  // Goal statuses
  backlog: "bg-zinc-100 text-zinc-600 dot-zinc",
  active: "bg-blue-50 text-blue-700 dot-blue",
  review: "bg-purple-50 text-purple-700 dot-purple",
  done: "bg-emerald-50 text-emerald-700 dot-emerald",
  failed: "bg-red-50 text-red-700 dot-red",
  cancelled: "bg-zinc-100 text-zinc-400 dot-zinc",
  // Run statuses
  queued: "bg-zinc-100 text-zinc-600 dot-zinc",
  running: "bg-green-50 text-green-700 dot-green",
  completed: "bg-emerald-50 text-emerald-700 dot-emerald",
};

const STATUS_DOTS: Record<string, string> = {
  "dot-zinc": "bg-zinc-400",
  "dot-blue": "bg-blue-500",
  "dot-amber": "bg-amber-500",
  "dot-purple": "bg-purple-500",
  "dot-emerald": "bg-emerald-500",
  "dot-red": "bg-red-500",
  "dot-green": "bg-green-500",
};

export function Badge({ status, className }: { status: string; className?: string }) {
  const colors = STATUS_COLORS[status] ?? "bg-zinc-100 text-zinc-600 dot-zinc";
  const dot = STATUS_DOTS[colors.split(" ").find((c) => c.startsWith("dot-")) ?? "dot-zinc"] ?? "bg-zinc-400";
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 px-2.5 py-0.5 text-xs font-medium rounded-full",
        colors.replace(/ dot-\S+/, ""),
        className
      )}
    >
      <span className={cn("h-1.5 w-1.5 rounded-full", dot, status === "running" && "animate-pulse")} />
      {status}
    </span>
  );
}

// ── Attention chip ──
// The v2 OwnerAttention (goal.attention, derived by the Coordinator): what
// the goal's owner is being woken for. '' renders nothing.
const ATTENTION_LABELS: Record<string, { label: string; cls: string }> = {
  integration: { label: "待集成变更", cls: "bg-blue-50 text-blue-700 border-blue-200" },
  recovery: { label: "需处理失败", cls: "bg-amber-50 text-amber-700 border-amber-200" },
  user_action: { label: "待人工决策", cls: "bg-red-50 text-red-700 border-red-200" },
};

export function AttentionChip({ attention, className }: { attention: string; className?: string }) {
  if (!attention) return null;
  const chips = attention
    .split(",")
    .map((a) => ATTENTION_LABELS[a])
    .filter(Boolean);
  if (chips.length === 0) return null;
  return (
    <>
      {chips.map((c) => (
        <span
          key={c.label}
          className={cn(
            "inline-flex items-center gap-1 px-2 py-0.5 text-[11px] font-medium rounded-full border",
            c.cls,
            className
          )}
        >
          <span className="h-1.5 w-1.5 rounded-full bg-current animate-pulse" />
          {c.label}
        </span>
      ))}
    </>
  );
}

// ── Form primitives ──
export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <label className="block space-y-1">
      <span className="text-xs font-medium text-zinc-500">{label}</span>
      {children}
      {hint && <span className="block text-xs text-zinc-400">{hint}</span>}
    </label>
  );
}

export const inputCls =
  "w-full px-3 py-2 border border-zinc-300 rounded-lg text-sm bg-white text-zinc-900 placeholder:text-zinc-400 focus:border-indigo-400 focus:ring-2 focus:ring-indigo-100 outline-none transition";

// ── Dialog ──
export function Dialog({
  title,
  onClose,
  children,
  footer,
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  footer: React.ReactNode;
}) {
  // Esc to close.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Backdrop-click-to-close requires BOTH press and release on the backdrop.
  // A text-selection drag that starts inside the panel and releases over the
  // backdrop fires a click on their common ancestor (the backdrop) — with a
  // naive onClick that would close the dialog mid-selection ("selecting all
  // closes the dialog").
  const pressedOnBackdrop = useRef(false);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-zinc-900/40 backdrop-blur-sm p-4"
      onMouseDown={(e) => {
        pressedOnBackdrop.current = e.target === e.currentTarget;
      }}
      onClick={() => {
        if (pressedOnBackdrop.current) onClose();
      }}
    >
      <div
        className="w-full max-w-md bg-white rounded-2xl shadow-2xl border border-zinc-200 flex flex-col max-h-[90vh] page-enter"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-zinc-100">
          <h2 className="text-base font-semibold text-zinc-900">{title}</h2>
          <button
            onClick={onClose}
            className="text-zinc-400 hover:text-zinc-700 text-lg leading-none px-1"
            aria-label="关闭"
          >
            ×
          </button>
        </div>
        <div className="p-5 space-y-4 overflow-y-auto">{children}</div>
        <div className="flex justify-end gap-2 px-5 py-3.5 border-t border-zinc-100 bg-zinc-50/50">
          {footer}
        </div>
      </div>
    </div>
  );
}

// ── Confirm Dialog ──
export function ConfirmDialog({
  title,
  message,
  onConfirm,
  onClose,
  loading,
}: {
  title: string;
  message: string;
  onConfirm: () => void;
  onClose: () => void;
  loading?: boolean;
}) {
  return (
    <Dialog
      title={title}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button variant="danger" onClick={onConfirm} disabled={loading}>
            {loading ? "…" : "确认删除"}
          </Button>
        </>
      }
    >
      <p className="text-sm text-zinc-600">{message}</p>
    </Dialog>
  );
}

// ── Page header ──
export function PageHeader({
  title,
  action,
}: {
  title: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between mb-5">
      <h1 className="text-lg font-semibold text-zinc-900">{title}</h1>
      {action}
    </div>
  );
}

// ── Empty state ──
export function Empty({ children, icon = "🌱" }: { children: React.ReactNode; icon?: string }) {
  return (
    <div className="flex flex-col items-center justify-center py-14 text-center">
      <span className="text-3xl mb-2 opacity-70">{icon}</span>
      <p className="text-sm text-zinc-400">{children}</p>
    </div>
  );
}

// ── Skeleton ──
export function Skeleton({ className }: { className?: string }) {
  return (
    <div
      className={cn(
        "animate-pulse rounded-md bg-zinc-100",
        className
      )}
    />
  );
}
