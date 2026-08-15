"use client";

import { useState } from "react";
import { useSkills, useCreateSkill, useDeleteSkill } from "@/lib/queries";
import { PageHeader, Empty, Button, Dialog, Field, inputCls } from "@/components/ui";

export default function SkillsPage() {
  const { data: skills, isLoading } = useSkills();
  const del = useDeleteSkill();
  const [showCreate, setShowCreate] = useState(false);

  return (
    <div className="p-6 max-w-4xl mx-auto space-y-4">
      <PageHeader title="Skills" />
      <p className="text-sm text-gray-500 -mt-3">
        平台技能库：上传 skill 包（SKILL.md 指令），勾选给 agent 后下发到它所在机器（agentwork-&lt;名称&gt;/ 命名空间，不碰你手动装的 skills）。
      </p>
      <div className="mb-4">
        <Button onClick={() => setShowCreate(true)}>上传 Skill</Button>
      </div>

      {isLoading ? (
        <p className="text-gray-500 text-sm">加载中…</p>
      ) : !skills?.length ? (
        <Empty>技能库为空——上传一个 skill 包开始。</Empty>
      ) : (
        <div className="space-y-3">
          {skills.map((sk) => (
            <div key={sk.id} className="rounded-xl border border-zinc-200/80 bg-white shadow-sm p-4">
              <div className="flex items-center gap-3">
                <span className="font-medium text-sm text-zinc-900">{sk.name}</span>
                {sk.description && <span className="text-xs text-zinc-500">— {sk.description}</span>}
                <span className="ml-auto text-xs text-zinc-400">{new Date(sk.created_at).toLocaleString("zh-CN")}</span>
                <Button
                  variant="danger"
                  className="!px-2.5 !py-1 text-xs"
                  disabled={del.isPending}
                  onClick={() => {
                    if (confirm(`删除 skill「${sk.name}」？`)) del.mutate(sk.id);
                  }}
                >
                  删除
                </Button>
              </div>
              {del.isError && <p className="text-xs text-red-500 mt-1">{String(del.error)}</p>}
            </div>
          ))}
        </div>
      )}

      {showCreate && <CreateSkillDialog onClose={() => setShowCreate(false)} />}
    </div>
  );
}

// CreateSkillDialog uploads a skill package: the SKILL.md instructions are
// the core; extra files (scripts, references) come as repeated
// "=== <path>" blocks in the same textarea.
function CreateSkillDialog({ onClose }: { onClose: () => void }) {
  const create = useCreateSkill();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [filesText, setFilesText] = useState("=== SKILL.md\n\n");

  const parseFiles = (): Record<string, string> | null => {
    const files: Record<string, string> = {};
    const blocks = filesText.split(/^=== /m);
    for (const block of blocks) {
      const nl = block.indexOf("\n");
      if (nl <= 0) continue;
      const path = block.slice(0, nl).trim();
      if (!path) continue;
      files[path] = block.slice(nl + 1).replace(/\n$/, "");
    }
    return files;
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const files = parseFiles();
    if (!files) return;
    create.mutate(
      { name, description, files },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="上传 Skill"
      onClose={onClose}
      wide
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="skill-form" disabled={create.isPending || !name.trim()}>
            {create.isPending ? "上传中…" : "上传"}
          </Button>
        </>
      }
    >
      <form id="skill-form" onSubmit={submit} className="space-y-4">
        <Field label="名称" hint="agent 看到的名字；机器上安装为 agentwork-<名称>/">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required placeholder="code-review-checklist" />
        </Field>
        <Field label="描述（可选）">
          <input value={description} onChange={(e) => setDescription(e.target.value)} className={inputCls} placeholder="一句话说明这个 skill 干什么" />
        </Field>
        <Field label="文件" hint="=== 路径 开头分段；SKILL.md 必填。脚本/参考文件同样格式追加。">
          <textarea
            value={filesText}
            onChange={(e) => setFilesText(e.target.value)}
            className={`${inputCls} font-mono`}
            rows={14}
            placeholder={"=== SKILL.md\n\n<skill 的指令：什么时候用、怎么用>…\n\n=== scripts/check.sh\n#!/bin/sh\n…"}
          />
        </Field>
        {create.isError && <p className="text-sm text-red-500">{String(create.error)}</p>}
      </form>
    </Dialog>
  );
}
