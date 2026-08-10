"use client";

import { useState, useRef, useEffect } from "react";
import { useGoalComments, useCreateGoalComment, useAgents } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Button, Empty, inputCls } from "@/components/ui";
import type { Comment } from "@/lib/types";

export function GoalComments({ goalId }: { goalId: string }) {
  const { data: comments, isLoading } = useGoalComments(goalId);
  const createComment = useCreateGoalComment();
  const { data: agents } = useAgents();
  const [text, setText] = useState("");
  const [liveComments, setLiveComments] = useState<Comment[]>([]);
  const scrollRef = useRef<HTMLDivElement>(null);

  // Agent id → name (mentions and authors display as names, not 32-hex ids).
  const agentName = (id: string) => agents?.find((a) => a.id === id)?.name ?? id.slice(0, 8);

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

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!text.trim()) return;
    createComment.mutate(
      { goalId, author_type: "human", author_id: "ui", content: text },
      { onSuccess: () => setText("") }
    );
  };

  return (
    <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
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
            return (
              <div key={c.id} className="bg-white rounded-lg border border-zinc-100 p-3">
                <div className="flex items-center gap-2 mb-1">
                  <span className="text-sm font-medium text-zinc-800">{author}</span>
                  <span className="text-xs px-1.5 py-0.5 rounded bg-zinc-100 text-zinc-500">
                    {c.author_type === "human" ? "human" : c.author_type === "system" ? "system" : "agent"}
                  </span>
                  <span className="text-xs text-zinc-400">
                    {c.created_at ? new Date(c.created_at).toLocaleString("zh-CN") : ""}
                  </span>
                </div>
                <p className="text-sm text-zinc-600 whitespace-pre-wrap">
                  {renderCommentContent(c.content, agentName)}
                </p>
              </div>
            );
          })
        )}
      </div>

      {/* Comment input */}
      <form onSubmit={handleSubmit} className="border-t border-zinc-100 p-4 flex gap-2">
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="添加评论…"
          className={inputCls}
        />
        <Button type="submit" disabled={createComment.isPending || !text.trim()}>
          {createComment.isPending ? "…" : "发送"}
        </Button>
      </form>

      {createComment.isError && (
        <p className="px-4 pb-2 text-sm text-red-500">{String(createComment.error)}</p>
      )}
    </div>
  );
}

// renderCommentContent renders a comment body: structured mention URIs
// ([@Name](mention://agent/<id>)) become highlighted @-chips instead of raw
// markdown — the agent-to-agent handoff reads as "请 @reviewer 审查" not
// "[@reviewer](mention://agent/1399…)".
function renderCommentContent(content: string, agentName: (id: string) => string): React.ReactNode {
  const parts = content.split(/(\[@[^\]]+\]\(mention:\/\/[^)]+\))/g);
  return parts.map((part, i) => {
    const m = part.match(/^\[@([^\]]+)\]\(mention:\/\/(agent|squad|human|all)\/([^)]+)\)$/);
    if (!m) return part;
    const name = m[1] === "all" ? "all" : m[2] === "agent" ? agentName(m[3]) : m[1];
    return (
      <span
        key={i}
        className="inline-flex items-center px-1.5 py-0.5 rounded bg-purple-50 text-purple-700 ring-1 ring-purple-200 text-xs font-medium mx-0.5"
        title={`mention ${m[2]}`}
      >
        @{name}
      </span>
    );
  });
}
