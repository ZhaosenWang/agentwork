"use client";

import { useEffect, useRef, useState } from "react";
import { useTaskMessages, useTaskEvents } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Empty } from "@/components/ui";
import type { ChatMessage } from "@/lib/types";

interface StreamEntry {
  key: string;
  role: string;
  content: string;
}

function entryFromMessage(m: ChatMessage): StreamEntry {
  if (m.role === "tool" && m.tool_calls && m.tool_calls !== "[]") {
    return { key: m.id, role: "tool", content: m.tool_calls };
  }
  return { key: m.id, role: m.role, content: m.content };
}

const ROLE_STYLE: Record<string, string> = {
  assistant: "text-zinc-800",
  thought: "text-zinc-400 italic",
  tool: "text-blue-600 font-mono text-xs",
  user: "text-zinc-600",
  system: "text-amber-600 text-xs",
};

const ROLE_LABEL: Record<string, string> = {
  assistant: "assistant",
  thought: "thought",
  tool: "tool",
  user: "user",
  system: "system",
};

export function TaskStream({ taskId }: { taskId: string }) {
  useTaskEvents();
  const { data: history } = useTaskMessages(taskId);
  const [live, setLive] = useState<StreamEntry[]>([]);
  const scrollRef = useRef<HTMLDivElement>(null);
  const seq = useRef(0);

  useEffect(() => {
    setLive([]);
  }, [taskId]);

  useWSEvent("task:message", (p) => {
    if (p.task_id !== taskId) return;
    setLive((prev) => [...prev, { key: `live-${seq.current++}`, role: "assistant", content: String(p.text ?? "") }]);
  });
  useWSEvent("task:thought", (p) => {
    if (p.task_id !== taskId) return;
    setLive((prev) => [...prev, { key: `live-${seq.current++}`, role: "thought", content: String(p.text ?? "") }]);
  });
  useWSEvent("task:tool", (p) => {
    if (p.task_id !== taskId) return;
    setLive((prev) => [...prev, { key: `live-${seq.current++}`, role: "tool", content: String(p.tool ?? "") }]);
  });

  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [history, live]);

  const entries: StreamEntry[] = [
    ...(history?.map(entryFromMessage) ?? []),
    ...live,
  ];

  return (
    <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
      <div className="px-4 py-2.5 border-b border-zinc-100 text-xs font-medium text-zinc-500 uppercase tracking-wide">
        流式输出
      </div>
      <div ref={scrollRef} className="p-4 max-h-[60vh] overflow-y-auto space-y-2 text-sm bg-zinc-50/30">
        {entries.length === 0 ? (
          <Empty>还没有输出。任务运行后这里会实时显示 agent 的消息、思考和工具调用。</Empty>
        ) : (
          entries.map((e) => (
            <div key={e.key} className={ROLE_STYLE[e.role] ?? "text-zinc-800"}>
              <span className="text-xs text-zinc-300 mr-2 font-mono">[{ROLE_LABEL[e.role] ?? e.role}]</span>
              <span className="whitespace-pre-wrap break-words">{e.content}</span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
