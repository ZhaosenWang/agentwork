"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import { fetchLogs, getLogLevel, setLogLevel } from "@/lib/api";
import { useWSEvent } from "@/lib/ws";
import type { LogLine } from "@/lib/types";

const LEVELS = ["debug", "info", "warn", "error"] as const;

const LEVEL_STYLE: Record<string, string> = {
  debug: "text-zinc-400",
  info: "text-zinc-700",
  warn: "text-amber-600",
  error: "text-red-600 font-medium",
};

const LEVEL_DOT: Record<string, string> = {
  debug: "bg-zinc-300",
  info: "bg-sky-400",
  warn: "bg-amber-500",
  error: "bg-red-500",
};

// LogsPanel is the daemon log viewer: a live tail (WS log:line) plus
// time/level/keyword-filtered history (GET /logs). The level selector is
// ALSO the runtime knob — it PUTs /logs/level, which persists.
export default function LogsPage() {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [level, setLevel] = useState<string>("info");
  const [keyword, setKeyword] = useState("");
  const [after, setAfter] = useState("");
  const [before, setBefore] = useState("");
  const [paused, setPaused] = useState(false);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  const appendLive = useCallback((l: LogLine) => {
    if (pausedRef.current) return;
    setLines((prev) => [...prev.slice(-999), l]);
  }, []);

  useWSEvent("log:line", (p) => {
    const m = p as { ts?: string; level?: string; text?: string };
    if (typeof m?.text === "string") {
      appendLive({ ts: m.ts ?? "", level: m.level ?? "info", text: m.text });
    }
  });

  // Initial load + runtime level.
  useEffect(() => {
    setLoading(true);
    Promise.all([fetchLogs({ limit: 500, level }), getLogLevel().catch(() => null)])
      .then(([res, lv]) => {
        setLines(res.lines);
        if (lv) setLevel(lv.level);
      })
      .catch((e) => setErr(String(e)))
      .finally(() => setLoading(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Autoscroll on new lines (unless the user scrolled up).
  useEffect(() => {
    const el = listRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines]);

  const changeLevel = async (lv: string) => {
    setLevel(lv);
    try {
      const r = await setLogLevel(lv);
      setLevel(r.level);
    } catch (e) {
      setErr(String(e));
    }
    setLoading(true);
    try {
      const res = await fetchLogs({ limit: 500, level: lv });
      setLines(res.lines);
    } catch (e) {
      setErr(String(e));
    } finally {
      setLoading(false);
    }
  };

  const queryHistory = async () => {
    setLoading(true);
    setErr(null);
    try {
      const res = await fetchLogs({
        after: after || undefined,
        before: before || undefined,
        limit: 1000,
        level,
      });
      setLines(res.lines);
    } catch (e) {
      setErr(String(e));
    } finally {
      setLoading(false);
    }
  };

  const visible = lines.filter((l) => !keyword || l.text.toLowerCase().includes(keyword.toLowerCase()));

  return (
    <div className="p-6 max-w-5xl mx-auto space-y-4">
      <div className="flex items-center gap-3 flex-wrap">
        <h1 className="text-lg font-semibold text-zinc-900">日志</h1>
        <span className="text-xs text-zinc-400">~/.agentwork/daemon.log（10MB 滚动）</span>
        <div className="ml-auto flex items-center gap-2">
          <select
            value={level}
            onChange={(e) => changeLevel(e.target.value)}
            className="border border-zinc-200 rounded-lg px-2 py-1.5 text-xs bg-white"
            title="运行时日志级别（写盘过滤，重启后保持）"
          >
            {LEVELS.map((lv) => (
              <option key={lv} value={lv}>{lv}</option>
            ))}
          </select>
          <button
            onClick={() => setPaused(!paused)}
            className={`text-xs px-2.5 py-1.5 rounded-lg border ${paused ? "border-amber-300 bg-amber-50 text-amber-700" : "border-zinc-200 text-zinc-600 hover:bg-zinc-50"}`}
          >
            {paused ? "已暂停（点击继续）" : "暂停"}
          </button>
          <button
            onClick={() => setLines([])}
            className="text-xs px-2.5 py-1.5 rounded-lg border border-zinc-200 text-zinc-600 hover:bg-zinc-50"
          >
            清空
          </button>
        </div>
      </div>

      <div className="flex items-center gap-2 flex-wrap">
        <input
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder="关键词过滤…"
          className="border border-zinc-200 rounded-lg px-2.5 py-1.5 text-xs w-56"
        />
        <span className="text-xs text-zinc-500">从</span>
        <input
          type="datetime-local"
          value={after}
          onChange={(e) => setAfter(e.target.value)}
          className="border border-zinc-200 rounded-lg px-2 py-1.5 text-xs"
        />
        <span className="text-xs text-zinc-500">到</span>
        <input
          type="datetime-local"
          value={before}
          onChange={(e) => setBefore(e.target.value)}
          className="border border-zinc-200 rounded-lg px-2 py-1.5 text-xs"
        />
        <button
          onClick={queryHistory}
          disabled={loading}
          className="text-xs px-2.5 py-1.5 rounded-lg bg-indigo-600 text-white hover:bg-indigo-700 disabled:opacity-50"
        >
          {loading ? "查询中…" : "按时间查询"}
        </button>
        {(after || before) && (
          <button
            onClick={() => { setAfter(""); setBefore(""); queryHistory(); }}
            className="text-xs text-indigo-600 hover:underline"
          >
            重置时间
          </button>
        )}
      </div>

      {err && <p className="text-xs text-red-500">{err}</p>}

      <div ref={listRef} className="bg-zinc-950 rounded-xl border border-zinc-800 p-3 h-[calc(100vh-260px)] overflow-y-auto font-mono text-[11px] leading-relaxed">
        {visible.length === 0 && !loading && <p className="text-zinc-500 py-4 text-center">暂无日志</p>}
        {visible.map((l, i) => (
          <div key={i} className="flex gap-2 hover:bg-zinc-900/60 rounded px-1">
            <span className={`shrink-0 mt-1.5 h-1.5 w-1.5 rounded-full ${LEVEL_DOT[l.level] ?? LEVEL_DOT.info}`} />
            <span className="shrink-0 text-zinc-500">{l.ts ? new Date(l.ts).toLocaleString("zh-CN") : ""}</span>
            <span className={`break-all whitespace-pre-wrap ${LEVEL_STYLE[l.level] ?? LEVEL_STYLE.info}`}>{l.text}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
