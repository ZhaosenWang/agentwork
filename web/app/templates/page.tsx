"use client";

import { useState } from "react";
import { LayoutTemplate, Users, Boxes, RefreshCw } from "lucide-react";
import { Button, PageHeader, Empty, Badge } from "@/components/ui";
import { cn } from "@/lib/utils";
import { useTemplates, useRefreshTemplates } from "@/lib/queries";
import { ProjectTemplateDialog } from "@/components/project-template-dialog";
import { SquadTemplateDialog } from "@/components/squad-template-dialog";
import type { TemplateSummary } from "@/lib/types";

// 模板库入口：squad / project 两个 tab，卡片网格列出可用模板，点「应用」打开
// 对应的 apply 弹窗（弹窗内含列表→预览→覆盖表单→结果全流程）。
// 本页是自测前端的独立路由；AI_Shell_WEB 把弹窗挂在各实体页的「从模板创建」
// 按钮上，这里统一到一个入口便于本地联调。
export default function TemplatesPage() {
  const [kind, setKind] = useState<"project" | "squad">("project");
  const [open, setOpen] = useState<null | "project" | "squad">(null);
  const { data: templates, isLoading } = useTemplates(kind);
  const refresh = useRefreshTemplates();

  return (
    <div className="p-8">
      <PageHeader
        title="Templates"
        action={
          <Button
            variant="outline"
            onClick={() => refresh.mutate(undefined)}
            disabled={refresh.isPending}
            title="从模板仓库重新拉取"
          >
            <RefreshCw className={cn("h-3.5 w-3.5", refresh.isPending && "animate-spin")} />
            刷新模板仓库
          </Button>
        }
      />

      {/* kind 切换 */}
      <div className="flex gap-1 mb-5 p-1 bg-zinc-100 rounded-lg w-fit">
        <KindTab active={kind === "project"} onClick={() => setKind("project")} icon={Boxes} label="项目模板" />
        <KindTab active={kind === "squad"} onClick={() => setKind("squad")} icon={Users} label="小队模板" />
      </div>

      {refresh.data && (
        <p className="text-xs text-zinc-400 mb-3">
          已拉取 {refresh.data.fetched} 个模板
          {refresh.data.errors?.length ? `（${refresh.data.errors.length} 个错误）` : ""}
        </p>
      )}

      {isLoading ? (
        <div className="text-sm text-zinc-400 py-16 text-center">加载中…</div>
      ) : !templates || templates.length === 0 ? (
        <Empty icon="📦">
          暂无{kind === "project" ? "项目" : "小队"}模板 — 点击「刷新模板仓库」从 GitCode 拉取，
          或点下方「从模板创建」在弹窗内刷新。
        </Empty>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
          {templates.map((t) => (
            <TemplateRow key={t.id} t={t} onApply={() => setOpen(kind)} />
          ))}
        </div>
      )}

      {open === "project" && <ProjectTemplateDialog onClose={() => setOpen(null)} />}
      {open === "squad" && <SquadTemplateDialog onClose={() => setOpen(null)} />}
    </div>
  );
}

function KindTab({
  active,
  onClick,
  icon: Icon,
  label,
}: {
  active: boolean;
  onClick: () => void;
  icon: React.ComponentType<{ className?: string }>;
  label: string;
}) {
  return (
    <button
      onClick={onClick}
      className={cn(
        "flex items-center gap-1.5 px-3 py-1.5 rounded-md text-sm font-medium transition",
        active
          ? "bg-white text-indigo-700 shadow-sm"
          : "text-zinc-500 hover:text-zinc-700"
      )}
    >
      <Icon className="h-3.5 w-3.5" />
      {label}
    </button>
  );
}

function TemplateRow({ t, onApply }: { t: TemplateSummary; onApply: () => void }) {
  return (
    <div className="bg-white rounded-xl border border-zinc-200 p-4 flex flex-col gap-2 hover:border-indigo-200 hover:shadow-sm transition">
      <div className="flex items-start gap-2">
        {t.icon ? (
          <span className="text-2xl leading-none">{t.icon}</span>
        ) : (
          <span className="text-zinc-300"><LayoutTemplate className="h-6 w-6" /></span>
        )}
        <div className="flex-1 min-w-0">
          <div className="flex items-center justify-between gap-2">
            <h3 className="font-semibold text-sm text-zinc-900 truncate">{t.name}</h3>
            <span className="text-[10px] text-zinc-400 shrink-0">v{t.version}</span>
          </div>
          <p className="text-xs text-zinc-500 mt-0.5 line-clamp-3">{t.description}</p>
        </div>
      </div>
      {t.tags?.length ? (
        <div className="flex flex-wrap gap-1">
          {t.tags.map((tag) => (
            <span key={tag} className="rounded bg-zinc-100 px-1.5 py-0.5 text-[10px] text-zinc-500">{tag}</span>
          ))}
        </div>
      ) : null}
      <div className="flex items-center gap-2 mt-1">
        <Badge status={t.kind === "project-template" ? "active" : "review"} />
        <span className="text-[10px] text-zinc-400 font-mono truncate">{t.id}</span>
        <Button variant="outline" className="ml-auto py-1 text-xs" onClick={onApply}>
          应用
        </Button>
      </div>
    </div>
  );
}
