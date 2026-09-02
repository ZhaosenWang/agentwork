package daemon

import (
	"context"
	"strings"
)

// ── Chat platform brief (the chat surface's platform role) ──
//
// agentwork's chat is an ACP pass-through relay: the daemon does not build a
// prompt (unlike multica's buildChatPrompt); the machine only stages
// AGENTS.md + skills. The run path injects identity and tools via
// buildFixedBlock/buildRunProfile into AGENTS.md; the chat path has no such
// layer, so the agent does not know it is in the chat surface or that the
// `agentwork` CLI exists to query its history.
//
// buildChatBrief closes that gap: a session-fixed platform brief appended to
// the chat cwd's AGENTS.md (staged alongside the agent's system_prompt
// persona). It only points the agent at `agentwork agent history` to pull its
// history on demand (dual-track: passive brief + active CLI, the same shape
// as the run path's buildFixedBlock + goal comments). No task summary is
// injected: history grows over time and embedding it would bloat the
// session-fixed file; the agent pulls it when it needs it.
//
// The platform text is English (per the shared language policy); the
// agentName material is preserved as-is.

// buildChatBrief renders the chat-surface platform brief appended to the
// agent's AGENTS.md in the chat cwd. agentName is the agent's display name
// (may be empty — the brief stays valid without it).
func buildChatBrief(agentName string) string {
	var b strings.Builder
	b.WriteString("# Chat Context\n")
	b.WriteString("You are chatting with the user in agentwork's chat surface (not executing a goal).\n")
	b.WriteString("- Your identity and skills are loaded from this AGENTS.md and the .claude/skills/ (or runtime-native) directory.\n")
	b.WriteString("- You have done work before: run `agentwork agent history` to see your past goals/runs (use --limit and --status to filter).\n")
	b.WriteString("- This is an open conversation — coordinate freely; no goal/verification lifecycle applies here.\n")
	b.WriteString("- Use the `agentwork` CLI only for reading your own history; do not assign/handoff/cancel from chat (those are goal-run side effects).\n")
	if agentName != "" {
		b.WriteString("\nAgent: " + agentName + "\n")
	}
	return b.String()
}

// stewardChatBrief renders the steward's chat-surface brief: the intake
// schema + roster (so the steward can parse instructions conversationally)
// plus the text-marker protocol the daemon relay extracts. Falls back to
// the regular buildChatBrief when IntakeService is not wired or the roster
// query fails — a steward without intake is just a normal chat agent.
func (d *Daemon) stewardChatBrief(ctx context.Context, agentName string) string {
	if d.intakeSvc == nil {
		return buildChatBrief(agentName)
	}
	brief, err := d.intakeSvc.BuildStewardChatBrief(ctx)
	if err != nil || strings.TrimSpace(brief) == "" {
		return buildChatBrief(agentName)
	}
	return brief
}
