package daemon

import (
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
// (may be empty — the brief stays valid without it). isSteward omits the
// "do not assign/handoff/cancel" restriction (the steward exercises platform
// operations via CLI — 决策7-5).
func buildChatBrief(agentName string, isSteward bool) string {
	var b strings.Builder
	b.WriteString("# Chat Context\n")
	b.WriteString("You are chatting with the user in agentwork's chat surface (not executing a goal).\n")
	b.WriteString("- Your identity and skills are loaded from this AGENTS.md and the .claude/skills/ (or runtime-native) directory.\n")
	b.WriteString("- You have done work before: run `agentwork agent history` to see your past goals/runs (use --limit and --status to filter).\n")
	b.WriteString("- This is an open conversation — coordinate freely; no goal/verification lifecycle applies here.\n")
	if !isSteward {
		b.WriteString("- Use the `agentwork` CLI only for reading your own history; do not assign/handoff/cancel from chat (those are goal-run side effects).\n")
	}
	if agentName != "" {
		b.WriteString("\nAgent: " + agentName + "\n")
	}
	return b.String()
}

// stewardChatBriefAppendix is the extra brief section appended to the
// steward agent's AGENTS.md (on top of the regular buildChatBrief). It tells
// the steward it has platform-intake capabilities via the `agentwork` CLI.
// The steward exercises these by calling CLI subcommands through its
// terminal/bash tool; create operations go through POST /intake/dispatch →
// DispatchIntake (the same handlers + draft/merge ask-once as the IM
// path, 决策7-5). This replaces the former <<<INTAKE_JSON>>> text-marker
// protocol.
func stewardChatBriefAppendix() string {
	var b strings.Builder
	b.WriteString("\n# Steward Platform Capabilities\n")
	b.WriteString("You are the steward agent — in addition to normal chat, you can perform platform operations for the user by calling the `agentwork` CLI (output is JSON on stdout):\n")
	b.WriteString("- Create a task: `agentwork create goal --title \"...\" --assignee <agent-id-or-name> --domain <domain-id-or-name>`\n")
	b.WriteString("- Create an agent: `agentwork create agent --name \"...\" --runtime <runtime-id-or-name> --description \"...\"`\n")
	b.WriteString("- Create a squad: `agentwork create squad --name \"...\" --leader <agent-id-or-name>`\n")
	b.WriteString("- Create a domain: `agentwork create domain --name \"...\" --git_url <url>` (or --type scratch for no-repo)\n")
	b.WriteString("- Create a schedule: `agentwork create schedule --name \"...\" --title \"...\" --cron \"0 * * * *\" --assignee <id> --domain <id>`\n")
	b.WriteString("- Import a team: `agentwork create team --url <git-url>`\n")
	b.WriteString("- List tasks: `agentwork goal list`\n")
	b.WriteString("- List agents: `agentwork agent list`\n")
	b.WriteString("- List squads: `agentwork squad list`\n")
	b.WriteString("- Your own history: `agentwork agent history`\n")
	b.WriteString("\nWhen the user asks you to create something, call the CLI directly. If required fields are missing, the CLI returns a structured {\"status\":\"need_fields\",\"missing\":[...],\"message\":\"...\",\"platform_hint\":\"<system-reminder>...</system-reminder>\"} response. Relay `message` to the user (it lists what's missing). Read `platform_hint` yourself — it carries the current roster (available agents/domains/runtimes) so you can fill the missing fields or suggest options to the user. The <system-reminder> tags wrap platform instructions for you, NOT text to relay verbatim. On success the CLI returns {\"status\":\"created\",\"entity\":{...},\"message\":\"...\"} — parse entity.id for follow-up. You do NOT output any special markers.\n")
	return b.String()
}
