"use client";

import { useState, useEffect, useRef, useCallback } from "react";
import { MessageCircle, X, Trash2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui";
import { useSendIntake, useIntakeResult } from "@/lib/queries";

type Message = { role: "user" | "assistant"; content: string };

const HISTORY_KEY = "agentwork-chat-history";
const HISTORY_LIMIT = 100;

function loadHistory(): Message[] {
  try {
    const raw = localStorage.getItem(HISTORY_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) return parsed.slice(-HISTORY_LIMIT);
    return [];
  } catch {
    return [];
  }
}

function saveHistory(msgs: Message[]) {
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(msgs.slice(-HISTORY_LIMIT)));
  } catch {
    // quota exceeded or SSR — silently drop
  }
}

export function ChatAssistant() {
  const [open, setOpen] = useState(false);
  const [messages, setMessages] = useState<Message[]>(() => loadHistory());
  const [input, setInput] = useState("");
  const [pendingRunId, setPendingRunId] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);

  const send = useSendIntake();
  const result = useIntakeResult(pendingRunId);

  useEffect(() => {
    if (result.data && (result.data.status === "completed" || result.data.status === "failed")) {
      const reply = result.data.result_summary || "（无回复）";
      setMessages((prev) => [...prev, { role: "assistant", content: reply }]);
      setPendingRunId(null);
    }
  }, [result.data?.status]);

  useEffect(() => {
    scrollRef.current?.scrollTo(0, scrollRef.current.scrollHeight);
  }, [messages, pendingRunId]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  const busy = send.isPending || pendingRunId !== null;

  const clearHistory = useCallback(() => {
    setMessages([]);
    try { localStorage.removeItem(HISTORY_KEY); } catch {}
  }, []);

  useEffect(() => {
    saveHistory(messages);
  }, [messages]);

  const handleSend = () => {
    const text = input.trim();
    if (!text || busy) return;
    setMessages((prev) => [...prev, { role: "user", content: text }]);
    setInput("");
    send.mutate(text, {
      onSuccess: (data) => setPendingRunId(data.run_id),
      onError: (err) =>
        setMessages((prev) => [...prev, { role: "assistant", content: "发送失败：" + String(err) }]),
    });
  };

  if (!open) {
    return (
      <button
        onClick={() => setOpen(true)}
        className="fixed bottom-6 right-6 z-40 h-12 w-12 rounded-full bg-gradient-to-b from-indigo-600 to-violet-600 text-white shadow-lg shadow-indigo-500/30 flex items-center justify-center hover:scale-105 transition-transform"
        aria-label="助手"
      >
        <MessageCircle className="h-5 w-5" />
      </button>
    );
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-end justify-end p-6 bg-zinc-900/40 backdrop-blur-sm"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) setOpen(false);
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget) setOpen(false);
      }}
    >
      <div className="w-full max-w-md bg-white rounded-2xl shadow-2xl border border-zinc-200 flex flex-col max-h-[70vh]">
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-zinc-100">
          <h2 className="text-base font-semibold text-zinc-900">助手</h2>
          <div className="flex items-center gap-1">
            {messages.length > 0 && (
              <button
                onClick={clearHistory}
                className="text-zinc-400 hover:text-red-500 p-1 transition-colors"
                aria-label="清空记录"
                title="清空记录"
              >
                <Trash2 className="h-4 w-4" />
              </button>
            )}
            <button
              onClick={() => setOpen(false)}
              className="text-zinc-400 hover:text-zinc-700 text-lg leading-none px-1"
              aria-label="关闭"
            >
              <X className="h-4 w-4" />
            </button>
          </div>
        </div>

        <div ref={scrollRef} className="flex-1 overflow-y-auto px-5 py-4 space-y-3 min-h-[200px]">
          {messages.length === 0 && (
            <p className="text-sm text-zinc-400 text-center py-8">
              输入指令，如：
              <br />
              根据 https://gitcode.com/xiaozoom/demo-team.git 创建一个team
            </p>
          )}
          {messages.map((m, i) => (
            <div key={i} className={cn("flex", m.role === "user" ? "justify-end" : "justify-start")}>
              <div
                className={cn(
                  "max-w-[85%] rounded-lg px-3 py-2 text-sm whitespace-pre-wrap break-words",
                  m.role === "user"
                    ? "bg-indigo-600 text-white"
                    : "bg-zinc-100 text-zinc-800"
                )}
              >
                {m.content}
              </div>
            </div>
          ))}
          {busy && (
            <div className="flex justify-start">
              <div className="bg-zinc-100 rounded-lg px-3 py-2 text-sm text-zinc-400">
                处理中...
              </div>
            </div>
          )}
        </div>

        <div className="flex gap-2 px-5 py-3.5 border-t border-zinc-100 bg-zinc-50/50">
          <input
            className="flex-1 rounded-lg border border-zinc-300 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500 disabled:opacity-50"
            placeholder="输入指令..."
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                handleSend();
              }
            }}
            disabled={busy}
          />
          <Button onClick={handleSend} disabled={busy || !input.trim()}>
            发送
          </Button>
        </div>
      </div>
    </div>
  );
}
