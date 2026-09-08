"use client";

import { useState, useEffect, useRef, useCallback } from "react";
import { MessageCircle, X, Trash2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui";
import { AcpChatClient, ChatEvent, PermissionRequest } from "@/lib/acp";
import { StreamCards } from "@/lib/run-messages";
import { listAgents } from "@/lib/api";

// ChatAssistant — the floating steward chat bubble (决策7-5). Connects to
// the steward agent's ACP WebSocket (same protocol as chat-panel, different
// UI). Replaces the former POST /intake → EnqueueWeb → poll processor-run
// path (which was not streaming — "wait then dump all at once"). The steward
// exercises its platform-intake capabilities (create goal/agent/squad/...)
// via the `agentwork` CLI during the chat, same as chat-panel.

interface Msg extends ChatEvent {
  id: number;
}

const HISTORY_KEY = "agentwork-chat-history";
const HISTORY_LIMIT = 100;

function loadHistory(): Msg[] {
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

function saveHistory(msgs: Msg[]) {
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(msgs.slice(-HISTORY_LIMIT)));
  } catch {
    // quota exceeded or SSR — silently drop
  }
}

export function ChatAssistant() {
  const [open, setOpen] = useState(false);
  const [messages, setMessages] = useState<Msg[]>(() => loadHistory());
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [perm, setPerm] = useState<{ req: PermissionRequest; respond: (o: { optionId?: string; cancelled?: boolean }) => void } | null>(null);
  const [error, setError] = useState("");
  const [stewardId, setStewardId] = useState<string | null>(null);
  const [stewardMissing, setStewardMissing] = useState(false);
  const clientRef = useRef<AcpChatClient | null>(null);
  const sessionRef = useRef<string>("");
  // Monotonic message id — seeded past any id loaded from history so live
  // events never collide with persisted messages. Computed once from the
  // initial state (useState initializer), not a useEffect (StrictMode double
  // mount would reset a ref-based counter).
  const msgID = useRef<number>(
    Math.max(0, ...loadHistory().map((m) => m.id || 0)) + 1
  );
  const scrollRef = useRef<HTMLDivElement>(null);
  const agentActivity = useRef(false);
  // Mirror busy in a ref — the connect effect's cleanup closes over the
  // MOUNT-time value; without a ref it always sees false and skips the
  // cancel, leaving an orphaned in-flight turn on the machine side.
  const busyRef = useRef(false);
  useEffect(() => { busyRef.current = busy; }, [busy]);

  // Resolve the steward agent id once on first open.
  useEffect(() => {
    if (!open || stewardId || stewardMissing) return;
    listAgents()
      .then((agents) => {
        const steward = agents.find((a) => a.type === "steward");
        if (steward) {
          setStewardId(steward.id);
        } else {
          setStewardMissing(true);
        }
      })
      .catch(() => setStewardMissing(true));
  }, [open, stewardId, stewardMissing]);

  // Connect the ACP WebSocket when the steward id is known and the panel is open.
  useEffect(() => {
    if (!open || !stewardId) return;

    const client = new AcpChatClient({
      onEvent: push,
      onPermissionRequest: (req, respond) => setPerm({ req, respond }),
      onClose: () => {
        setError((e) => e || "连接已断开");
      },
    });
    clientRef.current = client;
    const host = window.location.host;
    client
      .connect(`ws${window.location.protocol === "https:" ? "s" : ""}://${host}/backend/agents/${stewardId}/acp`)
      .then(async () => {
        try {
          // Auto-create a session — no session pick UI (single continuous
          // conversation, the CLI's session store persists it across refreshes).
          const sid = await client.newSession();
          sessionRef.current = sid;
        } catch (e) {
          setError(`创建会话失败：${(e as Error).message}`);
        }
      })
      .catch((e) => setError(`连接失败：${e.message}`));

    return () => {
      if (busyRef.current && sessionRef.current) client.cancel(sessionRef.current);
      client.close();
      clientRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, stewardId]);

  const push = useCallback((ev: ChatEvent) => {
    msgID.current += 1;
    if (ev.kind !== "user") agentActivity.current = true;
    setMessages((prev) => {
      const last = prev[prev.length - 1];
      // Merge contiguous agent/thought chunks into one bubble (same as chat-panel).
      if (last && last.kind === ev.kind && (ev.kind === "agent" || ev.kind === "thought")) {
        return [...prev.slice(0, -1), { ...last, text: last.text + ev.text, id: last.id }];
      }
      // A tool_result merges into the preceding tool_use with the same callId.
      if (ev.kind === "tool" && ev.isResult && ev.callId) {
        const idx = prev.findLastIndex((m) => m.kind === "tool" && m.callId === ev.callId && !m.isResult);
        if (idx >= 0) {
          const next = [...prev];
          next[idx] = { ...next[idx], output: ev.output, isResult: true };
          return next;
        }
      }
      return [...prev, { ...ev, id: msgID.current }];
    });
  }, []);

  useEffect(() => {
    scrollRef.current?.scrollTo(0, scrollRef.current.scrollHeight);
  }, [messages, busy]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  useEffect(() => {
    saveHistory(messages);
  }, [messages]);

  const clearHistory = useCallback(() => {
    setMessages([]);
    try { localStorage.removeItem(HISTORY_KEY); } catch {}
  }, []);

  const send = async () => {
    const client = clientRef.current;
    const text = input.trim();
    if (!client || !sessionRef.current || !text || busy) return;
    setInput("");
    agentActivity.current = false;
    push({ kind: "user", text });
    setBusy(true);
    try {
      await client.prompt(sessionRef.current, text);
    } catch (e) {
      const msg = (e as Error).message;
      // Poisoned history: retry on a fresh session (same logic as chat-panel).
      if (!agentActivity.current) {
        push({ kind: "agent", text: "（会话历史已损坏——自动新建会话重试）" });
        try {
          const sid = await client.newSession();
          sessionRef.current = sid;
          await client.prompt(sid, text);
        } catch (e2) {
          setError(`发送失败：${(e2 as Error).message}`);
        }
        setBusy(false);
        return;
      }
      setError(`发送失败：${msg}`);
    }
    setBusy(false);
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
      onMouseDown={(e) => { if (e.target === e.currentTarget) setOpen(false); }}
      onClick={(e) => { if (e.target === e.currentTarget) setOpen(false); }}
    >
      <div className="w-full max-w-md bg-white rounded-2xl shadow-2xl border border-zinc-200 flex flex-col max-h-[70vh]">
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-zinc-100">
          <h2 className="text-base font-semibold text-zinc-900">管家</h2>
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
          {stewardMissing && (
            <p className="text-sm text-zinc-400 text-center py-8">
              管家未就绪——连接一台机器并重启服务后自动创建。
            </p>
          )}
          {!stewardMissing && messages.length === 0 && (
            <p className="text-sm text-zinc-400 text-center py-8">
              跟管家说点什么，比如：
              <br />
              <span className="text-zinc-500">帮我创建一个任务</span>
              <br />
              <span className="text-zinc-500">查看任务列表</span>
            </p>
          )}
          {messages.map((m) => {
            if (m.kind === "thought") {
              return (
                <StreamCards
                  key={m.id}
                  items={[{ kind: "thought", key: String(m.id), content: m.text, isStreaming: busy }]}
                />
              );
            }
            if (m.kind === "tool") {
              return (
                <StreamCards
                  key={m.id}
                  items={[{
                    kind: "tool",
                    key: m.callId ?? String(m.id),
                    toolName: m.toolName ?? "tool",
                    input: "",
                    output: m.output ?? "",
                    hasResult: !!m.isResult,
                  }]}
                />
              );
            }
            return (
              <div key={m.id} className={cn("flex", m.kind === "user" ? "justify-end" : "justify-start")}>
                <div
                  className={cn(
                    "max-w-[85%] rounded-lg px-3 py-2 text-sm whitespace-pre-wrap break-words",
                    m.kind === "user"
                      ? "bg-indigo-600 text-white"
                      : "bg-zinc-100 text-zinc-800"
                  )}
                >
                  {m.text}
                </div>
              </div>
            );
          })}
          {busy && (
            <div className="flex items-center gap-3">
              <span className="text-sm text-zinc-400">思考中…</span>
              <button
                onClick={() => {
                  if (clientRef.current && sessionRef.current) clientRef.current.cancel(sessionRef.current);
                }}
                className="text-xs text-red-500 hover:text-red-700"
              >
                停止
              </button>
            </div>
          )}
          {error && <p className="text-xs text-red-500">{error}</p>}
        </div>

        <div className="flex gap-2 px-5 py-3.5 border-t border-zinc-100 bg-zinc-50/50">
          <input
            className="flex-1 rounded-lg border border-zinc-300 px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-indigo-500 disabled:opacity-50"
            placeholder={stewardMissing ? "管家未就绪…" : "输入消息…"}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                send();
              }
            }}
            disabled={busy || stewardMissing}
          />
          <Button onClick={send} disabled={busy || !input.trim() || stewardMissing}>
            发送
          </Button>
        </div>
      </div>

      {perm && (
        <div className="fixed inset-0 bg-black/30 flex items-center justify-center z-50">
          <div className="bg-white rounded-xl border border-zinc-200 p-4 w-96 shadow-lg">
            <p className="text-sm font-medium text-zinc-800">管家请求权限</p>
            <p className="mt-1 text-xs text-zinc-500">
              {perm.req.toolCall?.title || "tool"}（{perm.req.toolCall?.kind || "unknown"}）
            </p>
            <div className="mt-3 flex gap-2 justify-end">
              {(perm.req.options ?? []).filter((o) => o.kind?.startsWith("allow")).map((o) => (
                <Button
                  key={o.optionId}
                  onClick={() => {
                    perm.respond({ optionId: o.optionId });
                    setPerm(null);
                  }}
                >
                  {o.name || "允许"}
                </Button>
              ))}
              {(perm.req.options ?? []).filter((o) => o.kind?.startsWith("reject")).map((o) => (
                <Button
                  key={o.optionId}
                  variant="outline"
                  onClick={() => {
                    perm.respond({ optionId: o.optionId });
                    setPerm(null);
                  }}
                >
                  {o.name || "拒绝"}
                </Button>
              ))}
              <Button
                variant="outline"
                onClick={() => {
                  perm.respond({ cancelled: true });
                  setPerm(null);
                }}
              >
                取消
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
