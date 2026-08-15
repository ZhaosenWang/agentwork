package daemon

import (
	"context"
	"strings"

	"github.com/eushing/agentwork/internal/service"
)

// ── The engineered context system (决策 6-22) ──
//
// Every prompt is assembled from exactly two pieces:
//
//   - the FIXED BLOCK (platform / goal / team / self / tools) — injected
//     once per (agent, goal) session, and per run for non-ACP runtimes
//     (every run is a fresh session there);
//   - the WAKE LINE (who woke you + anchor + reason/task) — injected every
//     turn.
//
// The comment feed is PULLED via agentwork_get_comments, never injected
// wholesale (决策 4-6 revised: the wake line carries the triggering words,
// the feed is the shared context the agent pulls on demand). AGENTWORK.md
// is retired — its content always came from DB fields (agent system_prompt,
// squad instructions, roster) and now rides the fixed block directly.

// buildFixedBlock renders the session-fixed context (决策 6-22): platform
// intro, goal, team, the agent's own identity + role contract, and the tool
// surface. Written in English (platform text is English, 决策 6-18); the
// MATERIALS (titles, instructions, names) keep their own language.
func (d *Daemon) buildFixedBlock(ctx context.Context, goalID, agentID, agentName, agentDesc, systemPrompt, runRole, goalTitle, policyText, domainType, domainName, worktreeRoot string, remote bool) string {
	// The team comes from the goal's own squad assignment ('' = solo). The
	// leader flag drives the owner contract's reviewer-only rule.
	var squadID string
	var leaderID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE((SELECT assignee_id FROM goal WHERE id=? AND assignee_type='squad'), '')`, goalID).Scan(&squadID); err != nil {
		squadID = ""
	}
	isLeader := false
	if squadID != "" {
		if err := d.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, squadID).Scan(&leaderID); err == nil {
			isLeader = leaderID == agentID
		}
	}
	var b strings.Builder
	b.WriteString("# Background & Requirements\n")
	b.WriteString("You are working on agentwork — a multi-agent collaboration platform.\n")
	b.WriteString("Goals are executed by agents; the platform judges completion (machine\n")
	b.WriteString("verification + gates), and humans approve at checkpoints. You\n")
	b.WriteString("coordinate ONLY through the agentwork MCP tools — structured side\n")
	b.WriteString("effects, never shell, never file edits to communicate intent.\n")
	b.WriteString("LANGUAGE: write every comment/report in the SAME language as the goal's\n")
	b.WriteString("description and the human's messages.\n")

	b.WriteString("\n# Goal\n")
	b.WriteString("- Title: " + goalTitle + "\n")
	if s := strings.TrimSpace(policyText); s != "" {
		b.WriteString("- Acceptance policy: " + s + "\n")
	}
	if domainType == "scratch" {
		// The scratch contract (决策 6-16): the project directory IS the
		// deliverable — files persist across turns, the feed is coordination.
		b.WriteString("This is a scratch project (no git repository): your artifacts live in\n")
		b.WriteString("the project directory (" + scratchGoalDir(domainName, goalID) + ") and\n")
		b.WriteString("survive between turns; the comment feed is only for coordination. The\n")
		b.WriteString("PARENT directory is human-maintained shared material: READ-ONLY. Write\n")
		b.WriteString("only inside this goal's directory.\n")
	}

	b.WriteString("\n# Team\n")
	if squadID == "" {
		b.WriteString("Working solo — no team on this goal.\n")
	} else {
		d.writeTeamBlock(ctx, &b, squadID, agentID, isLeader)
	}

	b.WriteString("\n# Who You Are\n")
	self := agentName
	if self == "" {
		self = "agent-" + short(agentID)
	}
	if remote {
		// The agent-level persona (description + system prompt) rides
		// AGENTS.md via config.push — the runtime's profile resolver loads
		// it natively; the per-run role contract stays in the prompt.
		b.WriteString(self)
		b.WriteString("\n")
		b.WriteString(roleContract(runRole, isLeader, squadID))
		return b.String()
	}
	b.WriteString(self)
	if s := strings.TrimSpace(agentDesc); s != "" {
		b.WriteString(" — " + s)
	}
	if s := strings.TrimSpace(systemPrompt); s != "" && s != agentDesc {
		b.WriteString("\n" + s)
	}
	b.WriteString("\n")
	b.WriteString(roleContract(runRole, isLeader, squadID))

	// Machine-executed runs (CLI 分支 Phase 2) get the CLI/native-tools
	// variant: the worktree is the process cwd, collaboration is the
	// agentwork CLI — no platform MCP servers are advertised there.
	if remote {
		b.WriteString("\n# Tools\n")
		b.WriteString("- Workspace: your working directory IS the worktree — use your own\n")
		b.WriteString("  file/terminal tools to read, write, and run commands directly.\n")
		b.WriteString("- Collaboration: run the `agentwork` CLI in your terminal (start with\n")
		b.WriteString("  `agentwork help`) — comments, consults, sub-goals, waiting, and\n")
		b.WriteString("  verdicts are structured side effects through it. NEVER use file\n")
		b.WriteString("  edits to communicate intent.\n")
		b.WriteString("- Feed: `agentwork goal comments [--after <id>]` — the comment feed\n")
		b.WriteString("  is the SHARED context. Pull it before acting when you lack\n")
		b.WriteString("  background; pass the last comment id you saw as --after for\n")
		b.WriteString("  incremental reads; if you do NOT remember what you have seen,\n")
		b.WriteString("  pull WITHOUT --after (full feed) — never guess an --after.\n")
		return b.String()
	}

	b.WriteString("\n# Tools\n")
	b.WriteString("- Workspace (worktree root: " + worktreeRoot + "): agentwork_read_file /\n")
	b.WriteString("  agentwork_write_file / agentwork_terminal_create → terminal_output →\n")
	b.WriteString("  terminal_release. Commands run on the PLATFORM machine, with the\n")
	b.WriteString("  worktree as the working directory. Your own local tools operate on\n")
	b.WriteString("  YOUR environment — NOT the worktree; remote runtimes cannot reach it\n")
	b.WriteString("  at all.\n")
	b.WriteString("- Collaboration: agentwork_comment_goal / agentwork_consult_agent /\n")
	b.WriteString("  agentwork_handoff_goal / agentwork_create_sub_goal /\n")
	b.WriteString("  agentwork_integrate_change / agentwork_get_change /\n")
	b.WriteString("  agentwork_get_sub_goal / agentwork_get_verification /\n")
	b.WriteString("  agentwork_cancel_sub_goal / agentwork_verify_sub_goal.\n")
	b.WriteString("- Feed: agentwork_get_comments(goal_id?, after?, limit?) — the comment\n")
	b.WriteString("  feed is the SHARED context. Pull it before acting when you lack\n")
	b.WriteString("  background. Pass the last comment id you saw as `after` for\n")
	b.WriteString("  incremental reads; if you do NOT remember what you have seen, pull\n")
	b.WriteString("  WITHOUT `after` (full feed) — never guess an `after`.\n")
	return b.String()
}

// writeTeamBlock renders the squad: members with roles, the owner's manual
// (squad instructions — the human-written team playbook from the web), and
// the id-resolution note. Members change over a goal's life; the snapshot
// comes with the resolve-by-tool note.
func (d *Daemon) writeTeamBlock(ctx context.Context, b *strings.Builder, squadID, selfAgentID string, isLeader bool) {
	var leaderID, instructions string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT leader_id, COALESCE(instructions,'') FROM squad WHERE id=?`, squadID).Scan(&leaderID, &instructions)
	// The leader is the squad's OWNER field, not necessarily a member row —
	// render it first so the leader sees itself (and everyone sees who runs
	// the squad); member rows follow (the leader row is skipped below).
	if leaderID != "" {
		var lname string
		_ = d.st.DB().QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, leaderID).Scan(&lname)
		marker := " (leader)"
		if leaderID == selfAgentID {
			marker = " (you, leader)"
		}
		b.WriteString("- " + lname + marker + "\n")
	}
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT m.member_id, a.name, COALESCE(m.role,'') FROM squad_member m
		 JOIN agent a ON a.id = m.member_id
		 WHERE m.squad_id=? AND m.member_type='agent'
		 ORDER BY m.created_at`, squadID)
	if err == nil {
		for rows.Next() {
			var id, name, role string
			if rows.Scan(&id, &name, &role) != nil {
				continue
			}
			if id == leaderID {
				continue // already rendered first
			}
			marker := ""
			if id == selfAgentID {
				marker = " (you)"
			}
			if role != "" {
				marker += " — " + role
			}
			b.WriteString("- " + name + marker + "\n")
		}
		rows.Close()
	}
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString("\nTeam playbook (written by the owner):\n" + s + "\n")
	}
	b.WriteString("\nResolve ids with agentwork_agent_list / agentwork_squad_list before\n")
	b.WriteString("consults and handoffs.\n")
}

// roleContract renders the behavioral contract for the run's role — the
// output rules that used to ride in AGENTWORK.md (决策 6-22).
func roleContract(runRole string, isLeader bool, squadID string) string {
	leader := isLeader || (squadID != "" && runRole == "owner")
	switch runRole {
	case "owner":
		s := "You are this goal's owner. Break the goal into work items with\n" +
			"agentwork_create_sub_goal and dispatch them (a sub-goal runs on its\n" +
			"own branch with machine verification and produces a Change the\n" +
			"platform wakes you to integrate). Ask teammates with\n" +
			"agentwork_consult_agent; transfer ownership with\n" +
			"agentwork_handoff_goal. Members are NOT auto-dispatched — you\n" +
			"delegate explicitly. Your final message becomes your run's report in\n" +
			"the feed. NEVER post your conclusions with agentwork_comment_goal and\n" +
			"then summarize them again in the final message — the report\n" +
			"double-posts (feed noise; a live failure: the delegation was\n" +
			"announced three times). After a dispatch-only turn keep the final\n" +
			"message MINIMAL (one short sentence) — never repeat the dispatch,\n" +
			"never write ids (goal/sub-goal/agent/squad ids are system handles,\n" +
			"the feed is read by humans). Completion is JUDGED, not declared:\n" +
			"the platform's machine\n" +
			"verification + gates + the human's approval decide — never set the\n" +
			"goal's status yourself.\n"
		if leader {
			s += "\nREVIEWER-ONLY RULE: members with role=\"reviewer\" REVIEW ONLY —\n" +
				"never dispatch work items to them (create_sub_goal rejects it).\n" +
				"After your turn ends the platform pulls the reviewers in\n" +
				"automatically; you never hand work to a reviewer.\n"
		}
		return s
	case "subgoal":
		return "You implement this work item. When the work is DONE, end your turn\n" +
			"with your final message — the platform posts it to the feed as your\n" +
			"report. NEVER post your conclusions with agentwork_comment_goal and\n" +
			"then summarize them again in the final message (the report\n" +
			"double-posts — feed noise). The platform machine-verifies your\n" +
			"branch and produces a Change; the OWNER integrates it. You do not\n" +
			"create sub-goals, hand off, or integrate — those are the owner's\n" +
			"tools.\n"
	case "consult":
		return "You are consulted (READ-ONLY): answer the question in your final\n" +
			"message — the platform posts it to the feed as your answer (do NOT\n" +
			"post a duplicate with agentwork_comment_goal). Do not modify files,\n" +
			"do not commit, do not execute the task itself — your edits are\n" +
			"discarded by the platform.\n"
	case "review":
		return "You REVIEW ONLY — give your opinion, never do the work. Inspect the\n" +
			"worktree's changes (diff, tests, quality) AND the goal's comment\n" +
			"feed (pull it with agentwork_get_comments). If the diff is empty,\n" +
			"the goal's deliverable lives in the feed — judge whether the goal\n" +
			"was actually fulfilled there, and say so explicitly; never report a\n" +
			"missing diff as the answer itself. End your turn with your opinion\n" +
			"as the final message — the platform posts it to the feed (the\n" +
			"approver reads it there; do NOT post a duplicate with\n" +
			"agentwork_comment_goal).\n"
	case "verify":
		return "You are the verifier for a work item: judge it, then issue\n" +
			"agentwork_verify_sub_goal(verdict, summary, evidence) ONCE and end\n" +
			"your turn. You may RUN tests, but never modify files or commit.\n"
	}
	return ""
}

// buildWakeLine renders the per-turn wake statement (决策 6-22): ONE
// unified shape for every wake path — "You were mentioned by <who>
// (comment <id>):" + the reason/task. The anchor is the handle the agent
// passes back to agentwork_get_comments(after=). English platform text;
// materials keep their language.
func buildWakeLine(anchorCommentID, who, content string) string {
	anchor := ""
	if anchorCommentID != "" {
		anchor = " (comment " + anchorCommentID + ")"
	}
	if who == "" {
		who = "the platform"
	}
	return "You were mentioned by " + who + anchor + ":\n\n" + content + "\n"
}

// short is the id-prefix helper (shared with notify-style display).
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

var _ = service.MaxMentionHints // keep the service import bound (hint lives in the wake line assembly)
