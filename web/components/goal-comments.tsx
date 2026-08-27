"use client";

import { useState, useRef, useEffect, useCallback } from "react";
import { useGoalComments, useCreateGoalComment, useAgents, useSquads } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Button, Empty } from "@/components/ui";
import { Markdown } from "@/components/markdown";
import type { Comment } from "@/lib/types";

export function GoalComments({ goalId }: { goalId: string }) {
  const { data: comments, isLoading } = useGoalComments(goalId);
  const createComment = useCreateGoalComment();
  const { data: agents } = useAgents();
  const [liveComments, setLiveComments] = useState<Comment[]>([]);
  const scrollRef = useRef<HTMLDivElement>(null);

  // Agent id → name (mentions and authors display as names, not 32-hex ids).
  // Soft-archive (plan §7.1): an archived agent is filtered out of GET /agents,
  // so the cache miss falls back to "已删除" rather than leaking a uuid slice.
  const agentName = (id: string) => agents?.find((a) => a.id === id)?.name ?? "已删除";

  // Reset live comments when goal changes
  useEffect(() => {
    setLiveComments([]);
  }, [goalId]);

  // Subscribe to comment:created events for this goal
  useWSEvent("comment:created", (p) => {
    if (p.goal_id !== goalId) return;
    setLiveComments((prev) => [...prev, p as unknown as Comment]);
  });

  // Auto-scroll
  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [comments, liveComments]);

  const allComments: Comment[] = [
    ...(comments ?? []),
    ...liveComments.filter((lc) => !comments?.some((c) => c.id === lc.id)),
  ];

  const [replyTo, setReplyTo] = useState<Comment | null>(null);

  const handleSubmit = useCallback((text: string) => {
    if (!text.trim()) return;
    createComment.mutate(
      {
        goalId,
        author_type: "human",
        author_id: "ui",
        content: text,
        parent_id: replyTo?.id,
      },
      { onSuccess: () => setReplyTo(null) }
    );
  }, [goalId, createComment, replyTo]);

  return (
    <div className="bg-white rounded-2xl border border-zinc-200/80 shadow-sm overflow-hidden">
      <div className="px-4 py-2.5 border-b border-zinc-100 text-xs font-medium text-zinc-500 uppercase tracking-wide">
        评论{allComments.length > 0 && `（${allComments.length}）`}
      </div>

      {/* Comment list */}
      <div ref={scrollRef} className="p-4 max-h-[40vh] overflow-y-auto space-y-3 bg-zinc-50/30">
        {isLoading ? (
          <div className="text-sm text-zinc-400 text-center py-8">加载中…</div>
        ) : allComments.length === 0 ? (
          <Empty>暂无评论。</Empty>
        ) : (
          allComments.map((c) => {
            const author =
              c.author_type === "human" ? "你" :
              c.author_type === "system" ? "系统" :
              agentName(c.author_id);
            const parent = c.parent_id ? allComments.find((p) => p.id === c.parent_id) : null;
            const parentAuthor = parent
              ? (parent.author_type === "human" ? "你" : parent.author_type === "system" ? "系统" : agentName(parent.author_id))
              : "";
            const parentText = parent
              ? parent.content.replace(/\[@[^\]]*\]\(mention:[^)]*\)/g, "").trim().slice(0, 60)
              : "";
            return (
              <div key={c.id} className="bg-white rounded-lg border border-zinc-100 p-3 group">
                <div className="flex items-center gap-2 mb-1">
                  <span className="text-sm font-medium text-zinc-800">{author}</span>
                  <span className="text-xs px-1.5 py-0.5 rounded bg-zinc-100 text-zinc-500">
                    {c.author_type === "human" ? "human" : c.author_type === "system" ? "system" : "agent"}
                  </span>
                  <span className="text-xs text-zinc-400">
                    {c.created_at ? new Date(c.created_at).toLocaleString("zh-CN") : ""}
                  </span>
                  <button
                    type="button"
                    onClick={() => setReplyTo(replyTo?.id === c.id ? null : c)}
                    className="ml-auto text-[11px] text-zinc-400 hover:text-indigo-600 opacity-0 group-hover:opacity-100 transition-opacity"
                  >
                    {replyTo?.id === c.id ? "取消回复" : "回复"}
                  </button>
                </div>
                {parent && (
                  <div className="mb-1.5 px-2 py-1 rounded bg-zinc-50 border-l-2 border-zinc-300 text-[11px] text-zinc-500 truncate">
                    回复 {parentAuthor}：{parentText}
                  </div>
                )}
                <Markdown
                  content={c.ask_human ? `[@你](mention://human/ui) ${c.content}` : c.content}
                  agentName={agentName}
                  className="text-zinc-600"
                />
              </div>
            );
          })
        )}
      </div>

      {/* Comment input — @ 自动弹出可 mention 列表 */}
      {replyTo && (
        <div className="px-4 pt-3 -mb-2 flex items-center gap-2 text-xs text-zinc-500">
          <span className="bg-indigo-50 text-indigo-700 border border-indigo-200 rounded-full px-2 py-0.5">
            回复 {replyTo.author_type === "human" ? "你" : replyTo.author_type === "system" ? "系统" : agentName(replyTo.author_id)}
          </span>
          <span className="truncate text-zinc-400">
            {replyTo.content.replace(/\[@[^\]]*\]\(mention:[^)]*\)/g, "").trim().slice(0, 40)}
          </span>
          <button type="button" onClick={() => setReplyTo(null)} className="ml-auto text-zinc-400 hover:text-zinc-600">✕</button>
        </div>
      )}
      <MentionComposer onSubmit={handleSubmit} pending={createComment.isPending} />

      {createComment.isError && (
        <p className="px-4 pb-2 text-sm text-red-500">{String(createComment.error)}</p>
      )}
    </div>
  );
}

// MentionComposer: comment input with @-autocomplete — typing @ pops the
// roster (agents + squads, filtered by what follows); ↑/↓ + Enter (or click)
// inserts the STRUCTURED mention URI [@Name](mention://agent|squad/<id>),
// the same shape the platform and agents produce.
function MentionComposer({
  onSubmit,
  pending,
}: {
  onSubmit: (text: string) => void;
  pending: boolean;
}) {
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const ref = useRef<HTMLTextAreaElement>(null);
  const [value, setValue] = useState("");
  const [cursor, setCursor] = useState(0);
  const [at, setAt] = useState(-1); // position of the active "@"
  const [query, setQuery] = useState("");
  const [index, setIndex] = useState(0);

  const roster = [
    ...(agents ?? []).map((a) => ({ id: a.id, name: a.name, type: "agent" })),
    ...(squads ?? []).map((s) => ({ id: s.id, name: s.name, type: "squad" })),
  ];
  const matches =
    at === -1 ? [] : roster.filter((r) => r.name.toLowerCase().includes(query.toLowerCase()));
  const menuOpen = at !== -1 && matches.length > 0;

  // Scan the text before the cursor: the last "@" (start-of-line or after
  // whitespace) with no whitespace between it and the cursor is an active
  // mention query.
  const scan = (value: string, pos: number) => {
    const before = value.slice(0, pos);
    const i = before.lastIndexOf("@");
    if (i === -1) return { at: -1, q: "" };
    if (i > 0 && !/\s/.test(before[i - 1])) return { at: -1, q: "" };
    const q = before.slice(i + 1);
    if (/\s/.test(q)) return { at: -1, q: "" };
    return { at: i, q };
  };

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const pos = e.target.selectionStart ?? e.target.value.length;
    setValue(e.target.value);
    setCursor(pos);
    const s = scan(e.target.value, pos);
    setAt(s.at);
    setQuery(s.q);
    setIndex(0);
  };

  const insertMention = (r: { id: string; name: string; type: string }) => {
    // 输入框存可读文本（@dev-team）；提交时转换为结构化 URI。
    const mention = `@${r.name} `;
    const next = value.slice(0, at) + mention + value.slice(cursor);
    setValue(next);
    setAt(-1);
    // Restore focus + cursor after the inserted mention.
    requestAnimationFrame(() => {
      const el = ref.current;
      if (el) {
        el.focus();
        const pos = at + mention.length;
        el.setSelectionRange(pos, pos);
      }
    });
  };

  // 提交时把 @name（roster 中的名字，长名优先防部分匹配）替换为结构化
  // mention URI——输入框里始终是可读文本，发出去的是真 URI。
  const buildContent = (text: string) => {
    let out = text;
    const sorted = [...roster].sort((a, b) => b.name.length - a.name.length);
    for (const r of sorted) {
      const re = new RegExp(`@${escapeRegExp(r.name)}(?![\\w-])`, "g");
      out = out.replace(re, `[@${r.name}](mention://${r.type}/${r.id})`);
    }
    return out;
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (!menuOpen) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setIndex((i) => (i + 1) % matches.length);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setIndex((i) => (i - 1 + matches.length) % matches.length);
    } else if (e.key === "Tab" || e.key === "Enter") {
      // Tab 快选（Slack/GitHub 习惯）+ Enter 确认
      e.preventDefault();
      insertMention(matches[index]);
    } else if (e.key === "Escape") {
      e.preventDefault();
      setAt(-1);
    }
  };

  const overlayRef = useRef<HTMLDivElement>(null);

  return (
    <div className="border-t border-zinc-100 p-4">
      <div className="relative">
        {/* 白底 + 边框由容器承担；textarea 透明背景。
            下层高亮层渲染与 textarea 完全相同的内容（字符一致 → 不错位），
            文字透明只露紫色背景——@名字在输入框里就是高亮的。 */}
        <div className="absolute inset-0 bg-white rounded-lg border border-zinc-300 transition group-focus-within:border-indigo-400 group-focus-within:ring-2 group-focus-within:ring-indigo-100" />
        <div
          ref={overlayRef}
          aria-hidden
          className="absolute inset-0 px-3 py-2 text-sm leading-6 text-transparent whitespace-pre-wrap break-words overflow-hidden pointer-events-none"
        >
          {highlightMentions(value, roster)}
        </div>
        <div className="relative group">
          <textarea
            ref={ref}
            value={value}
            onChange={handleChange}
            onKeyDown={handleKeyDown}
            onBlur={() => setAt(-1)}
            onScroll={(e) => {
              if (overlayRef.current) overlayRef.current.scrollTop = e.currentTarget.scrollTop;
            }}
            onFocus={(e) => {
              const pos = e.target.selectionStart ?? value.length;
              setCursor(pos);
              const s = scan(value, pos);
              setAt(s.at);
              setQuery(s.q);
            }}
            rows={2}
            placeholder="添加评论…（输入 @ 可提及 agent / squad）"
            className="relative w-full px-3 py-2 text-sm leading-6 bg-transparent text-zinc-900 placeholder:text-zinc-400 focus:outline-none resize-none"
          />
        </div>
        {menuOpen && (
          <div className="absolute left-0 right-0 bottom-full mb-1.5 bg-white rounded-xl border border-zinc-200 shadow-xl overflow-hidden z-20">
            {matches.map((r, i) => (
              <button
                key={r.type + r.id}
                type="button"
                onMouseDown={(e) => {
                  e.preventDefault(); // keep textarea focus
                  insertMention(r);
                }}
                className={`w-full text-left px-3 py-2 text-sm flex items-center gap-2.5 ${
                  i === index ? "bg-indigo-50 text-indigo-700" : "text-zinc-700"
                }`}
              >
                <span
                  className={`inline-block h-5 w-5 rounded-full text-[10px] leading-5 text-center font-medium shrink-0 ${
                    r.type === "squad" ? "bg-purple-100 text-purple-600" : "bg-indigo-100 text-indigo-600"
                  }`}
                >
                  {r.type === "squad" ? "队" : r.name.slice(0, 1).toUpperCase()}
                </span>
                {r.name}
                <span className="ml-auto text-[10px] text-zinc-400 uppercase">{r.type}</span>
              </button>
            ))}
          </div>
        )}
      </div>
      <div className="flex justify-end mt-2">
        <Button
          onClick={() => {
            onSubmit(buildContent(value));
            setValue("");
            setAt(-1);
          }}
          disabled={pending || !value.trim()}
          className={!value.trim() ? "opacity-40" : ""}
        >
          {pending ? "发送中…" : "发送"}
        </Button>
      </div>
    </div>
  );
}

// escapeRegExp escapes a roster name before it goes into a regex.
function escapeRegExp(s: string) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// highlightMentions renders the composer's value with roster @names wrapped
// in a background span — the SAME character sequence the textarea shows (so
// wrapping stays in lockstep), with only the mention words getting the
// purple highlight. The overlay's own text is transparent; the color blocks
// show through under the textarea's visible text.
function highlightMentions(text: string, roster: { name: string }[]): React.ReactNode[] {
  if (!text) return [];
  const names = [...roster.map((r) => r.name)].sort((a, b) => b.length - a.length);
  if (names.length === 0) return [text];
  const re = new RegExp(`@(${names.map(escapeRegExp).join("|")})(?![\\w-])`, "g");
  const parts = text.split(re);
  // split 保留捕获组：偶数下标 = 普通文本，奇数下标 = 匹配到的名字。
  return parts.map((part, i) => {
    if (i % 2 === 1) {
      // 只加背景色——padding/文字色都会让两层错位或叠字。
      // violet→purple 渐变（与紫色主按钮同系，但输入框内保持轻透）。
      return (
        <span key={i} className="rounded-sm bg-gradient-to-r from-violet-200 to-purple-200">
          @{part}
        </span>
      );
    }
    return part;
  });
}
