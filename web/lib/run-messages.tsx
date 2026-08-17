"use client";

import { useRef, useState } from "react";
import type { ChatMessage } from "@/lib/api";

// ── Run interaction stream grouping ──
//
// The flat chat_message rows (thought / tool_use / tool_result / assistant
// text) are grouped into render units: a thought, a tool card (use + result
// paired by call_id), or a bare text line. Shared by every real-time
// interaction surface (run-detail live stream, timeline side card, domains
// compile stream) so they all render the same collapsible cards:
//   - thought:  "+ thought" / "- thought"  (auto-collapses when the stream
//     moves past it; expanded while streaming)
//   - tool:     "+ {toolName}" / "- {toolName}"  (request + response areas)
//   - text:     bare, never folds
// No emoji (💭/⚙/🔧) — the title bar is plain text per the design.

export type RenderItem =
  | { kind: "thought"; key: string; content: string; isStreaming: boolean }
  | { kind: "tool"; key: string; toolName: string; input: string; output: string; hasResult: boolean }
  | { kind: "text"; key: string; content: string };

// groupMessages scans the flat chat_message stream in order and pairs each
// tool_use with its same-call_id tool_result into one tool card. A tool_use
// whose result has not arrived yet renders as a card with hasResult=false
// (the tool is still running).
export function groupMessages(messages: ChatMessage[]): RenderItem[] {
  const items: RenderItem[] = [];
  const pendingTools = new Map<string, RenderItem & { kind: "tool" }>();
  for (let i = 0; i < messages.length; i++) {
    const m = messages[i];
    const key = `${m.created_at}|${m.role}|${i}`;
    if (m.role === "tool" && m.tool_calls) {
      try {
        const tc = JSON.parse(m.tool_calls);
        if (tc.type === "tool_use") {
          const input = typeof tc.input === "string" ? tc.input : JSON.stringify(tc.input ?? "");
          const item: RenderItem & { kind: "tool" } = {
            kind: "tool", key, toolName: tc.tool ?? "tool", input, output: "", hasResult: false,
          };
          if (tc.call_id) pendingTools.set(String(tc.call_id), item);
          items.push(item);
          continue;
        }
        if (tc.type === "tool_result") {
          const out = typeof tc.output === "string" ? tc.output : JSON.stringify(tc.output ?? "");
          const cid = tc.call_id ? String(tc.call_id) : "";
          const paired = cid ? pendingTools.get(cid) : undefined;
          if (paired) {
            paired.output = out;
            paired.hasResult = true;
          } else {
            // result with no preceding use (orphan) — render as a standalone tool card
            items.push({ kind: "tool", key, toolName: tc.tool ?? "tool", input: "", output: out, hasResult: true });
          }
          continue;
        }
      } catch { /* not tool JSON — fall through to text */ }
    }
    if (m.role === "thought") {
      // isStreaming: this thought is the last message and no non-thought row
      // follows it — the agent is still thinking. Once a tool/text row lands
      // after it, the thought is done and auto-collapses.
      const isStreaming = i === messages.length - 1;
      items.push({ kind: "thought", key, content: m.content, isStreaming });
      continue;
    }
    // assistant / system / anything else → bare text
    items.push({ kind: "text", key, content: m.content });
  }
  return items;
}

// StreamCards renders grouped items as collapsible cards. `compact` picks
// the font size (the timeline side card is tighter than the run-detail
// panel). Collapse state rides a ref keyed by item identity so it survives
// the 1s throttled refetch that swaps the messages array (useState would
// reset every refresh).
export function StreamCards({ items, compact }: { items: RenderItem[]; compact?: boolean }) {
  const collapsedRef = useRef(new Map<string, boolean>());
  const [, forceTick] = useState(0);
  const force = () => forceTick((x) => x + 1);
  const sz = compact ? "text-[10px]" : "text-[11px]";

  const toggle = (key: string) => {
    const cur = collapsedRef.current.get(key) ?? false;
    collapsedRef.current.set(key, !cur);
    force();
  };

  return (
    <div className="space-y-1.5">
      {items.map((item) => {
        if (item.kind === "thought") {
          // Default: expanded while streaming, collapsed once done. A manual
          // toggle (map has the key) wins over the default.
          const collapsed = collapsedRef.current.get(item.key) ?? !item.isStreaming;
          return (
            <div key={item.key} className={sz}>
              <button
                onClick={() => toggle(item.key)}
                className="text-zinc-500 font-medium hover:text-zinc-700"
              >
                {collapsed ? "+" : "-"} thought
              </button>
              {!collapsed && (
                <div className="mt-0.5 pl-2 italic text-zinc-400 whitespace-pre-wrap">
                  {item.content}
                </div>
              )}
            </div>
          );
        }
        if (item.kind === "tool") {
          const collapsed = collapsedRef.current.get(item.key) ?? false;
          return (
            <div key={item.key} className={sz}>
              <button
                onClick={() => toggle(item.key)}
                className="text-purple-600 font-medium hover:text-purple-700"
              >
                {collapsed ? "+" : "-"} {item.toolName}
              </button>
              {!collapsed && (
                <div className="mt-0.5 pl-2 space-y-0.5">
                  {item.input && (
                    <div className="text-zinc-500 break-all">{item.input}</div>
                  )}
                  {item.hasResult ? (
                    <div className="text-zinc-400 break-all whitespace-pre-wrap">{item.output}</div>
                  ) : (
                    <div className="text-zinc-400 italic">运行中…</div>
                  )}
                </div>
              )}
            </div>
          );
        }
        if (!item.content) return null;
        return (
          <div key={item.key} className={`${sz} text-zinc-700 whitespace-pre-wrap`}>
            {item.content}
          </div>
        );
      })}
    </div>
  );
}
