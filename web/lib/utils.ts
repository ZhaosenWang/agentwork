import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// displayName renders an entity's name with a "（已删除）" tag when it has been
// soft-archived (archived_at set). Soft-archive keeps the row readable, so the
// original name is always available; this tag is the user-visible signal that
// the agent/squad is no longer active. Used by the agentName/squadName fallbacks
// across the timeline, goal lists, squad rosters, and schedule pages. A bare
// "已删除" is returned only by the caller when the id is not in the cache at all
// (a truly unknown id — e.g. a domain that was hard-deleted, never an archived
// agent/squad which the include_archived list always resolves).
export function displayName(name: string, archivedAt?: string): string {
  return archivedAt ? `${name}（已删除）` : name;
}
