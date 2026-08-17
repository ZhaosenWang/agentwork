package daemon

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/eushing/agentwork/internal/service"
)

// ── The engineered context system (决策 6-22) ──
//
// Every prompt is assembled from exactly two pieces:
//
//   - the FIXED BLOCK (platform / goal / self / tools) — the task message;
//   - the WAKE LINE (who woke you + anchor + reason/task) — injected every
//     turn.
//
// The TEAM (squad roster + playbook + leader protocol) is per-run PERSONA
// material: it rides the workdir's AGENTS.md (shipped in the dispatch
// payload, merged by the executor at spawn — see buildTeamProfile), never
// the prompt. The comment feed is PULLED via `agentwork goal comments`,
// never injected wholesale (决策 4-6 revised: the wake line carries the
// triggering words, the feed is the shared context the agent pulls on
// demand).

// buildFixedBlock renders the session-fixed context (决策 6-22): platform
// intro, goal, the agent's own identity + role contract, and the CLI tool
// surface. Written in English (platform text is English, 决策 6-18); the
// MATERIALS (titles, instructions, names) keep their own language.
func (d *Daemon) buildFixedBlock(ctx context.Context, goalID, agentID, agentName, runRole, goalTitle, policyText, domainType, domainName string) string {
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
	b.WriteString("verification + gates), and the user approves at checkpoints. You\n")
	b.WriteString("coordinate ONLY through the `agentwork` CLI — structured side effects,\n")
	b.WriteString("never shell, never file edits to communicate intent.\n")
	b.WriteString("LANGUAGE: write every comment/report in the SAME language as the goal's\n")
	b.WriteString("description and the user's messages.\n")

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
		b.WriteString("PARENT directory is user-maintained shared material: READ-ONLY. Write\n")
		b.WriteString("only inside this goal's directory.\n")
	}

	b.WriteString("\n# Who You Are\n")
	self := agentName
	if self == "" {
		self = "agent-" + short(agentID)
	}
	// The agent-level persona (system prompt) AND the team (squad roster +
	// briefing) ride AGENTS.md — the runtime's profile resolver loads them
	// natively; the per-run role contract stays here.
	b.WriteString(self)
	b.WriteString("\n")
	b.WriteString(roleContract(runRole, isLeader, squadID))

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

// buildRunProfile assembles the run's whole AGENTS.md layer: the fixed
// block (platform background, goal, role contract, tool surface) plus the
// team profile. Shipped in the dispatch payload and merged into the
// workdir's AGENTS.md at spawn — the user message carries the task only.
func (d *Daemon) buildRunProfile(ctx context.Context, goalID, agentID, agentName, runRole, goalTitle, policyText, domainType, domainName, issueSection string) string {
	block := d.buildFixedBlock(ctx, goalID, agentID, agentName, runRole, goalTitle, policyText, domainType, domainName)
	// The public-issue contract + the remote conversation snapshot are
	// CONTEXT (standing rules for this run), not the task — they ride
	// AGENTS.md like the rest of the profile.
	if issueSection != "" {
		block += "\n\n" + issueSection
	}
	if team := d.buildTeamProfile(ctx, goalID, agentID); team != "" {
		return block + "\n\n" + team
	}
	return block
}

// buildTeamProfile assembles the per-run TEAM context — squad roster,
// playbook, and (for the leader) the operating protocol. It rides the
// workdir's AGENTS.md (shipped in the dispatch payload, merged by the
// executor at spawn), NOT the prompt: team structure is persona material,
// stable across a goal's turns. '' = solo run.
func (d *Daemon) buildTeamProfile(ctx context.Context, goalID, agentID string) string {
	var squadID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT COALESCE((SELECT assignee_id FROM goal WHERE id=? AND assignee_type='squad'), '')`, goalID).Scan(&squadID); err != nil || squadID == "" {
		return ""
	}
	var leaderID string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, squadID).Scan(&leaderID)
	isLeader := leaderID == agentID
	if isLeader {
		// The leader gets the full operating protocol (roster + delegation
		// rules + reviewer rule + playbook) from the squad service.
		if d.squadSvc == nil {
			return ""
		}
		brief, err := d.squadSvc.BuildLeaderBriefing(ctx, squadID, true)
		if err != nil {
			return ""
		}
		return brief
	}
	// Members see the roster + playbook (their role contract is in the prompt).
	var b strings.Builder
	b.WriteString("# Team\n\n")
	d.writeTeamBlock(ctx, &b, squadID, agentID, false)
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
		b.WriteString("- " + lname + marker + d.agentSkillLine(ctx, leaderID) + "\n")
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
			b.WriteString("- " + name + marker + d.agentSkillLine(ctx, id) + "\n")
		}
		rows.Close()
	}
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString("\nTeam playbook (written by the owner):\n" + s + "\n")
	}
	b.WriteString("\nResolve ids with `agentwork agent list` / `agentwork squad list`\n")
	b.WriteString("before consults and handoffs.\n")
}

// agentSkillLine renders an agent's selected skill names for the roster
// ('' = none) — the leader divides work by what members can actually do.
func (d *Daemon) agentSkillLine(ctx context.Context, agentID string) string {
	var raw string
	if err := d.st.DB().QueryRowContext(ctx, `SELECT skills FROM agent WHERE id=?`, agentID).Scan(&raw); err != nil || raw == "" || raw == "[]" {
		return ""
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil || len(ids) == 0 {
		return ""
	}
	var names []string
	for _, id := range ids {
		var name string
		if err := d.st.DB().QueryRowContext(ctx, `SELECT name FROM skill WHERE id=?`, id).Scan(&name); err == nil && name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " — skills: " + strings.Join(names, ", ")
}

// roleContract renders the behavioral contract for the run's role — the
// output rules that used to ride in AGENTWORK.md (决策 6-22).
func roleContract(runRole string, isLeader bool, squadID string) string {
	leader := isLeader || (squadID != "" && runRole == "owner")
	switch runRole {
	case "owner":
		s := "You are this goal's owner. Break the goal into work items and\n" +
			"dispatch them: `agentwork subgoal create --title T --assignee <agent-id>\n" +
			"[--description D]` (a sub-goal runs on its own branch with machine\n" +
			"verification and produces a Change the platform wakes you to\n" +
			"integrate with `agentwork change integrate <id>`). Ask teammates\n" +
			"(read-only consult) by commenting a mention:\n" +
			"`agentwork goal comment --text \"[@Name](mention://agent/<id>)\"`.\n" +
			"Ask the user (the goal creator) a question with --ask:\n" +
			"`agentwork goal comment --text \"your question\" --ask` — the\n" +
			"platform notifies them; their reply wakes you (NOT a consult), so\n" +
			"your session and worktree persist across the round-trip. Use --ask\n" +
			"only when you genuinely need the user's input to proceed.\n" +
			"Transfer ownership with `agentwork goal assign <agent-id>`. Members\n" +
			"are NOT auto-dispatched — you delegate explicitly. Your final message becomes your run's report in the feed (the platform posts it). NEVER\n" +
			"post your conclusions with `agentwork goal comment` and then\n" +
			"summarize them again in the final message — the report double-posts\n" +
			"(feed noise;\n" +
			"a live failure: the delegation was announced three times). After a\n" +
			"dispatch-only turn keep the final message MINIMAL (one short\n" +
			"sentence) — never repeat the dispatch, never write ids\n" +
			"(goal/sub-goal/agent/squad ids are system handles, the feed is\n" +
			"read by the user). Completion is JUDGED, not declared: the\n" +
			"platform's machine verification + gates + the user's approval\n" +
			"decide — never set the goal's status yourself.\n"
		if leader {
			s += "\nREVIEWER-ONLY RULE: members with role=\"reviewer\" REVIEW ONLY —\n" +
				"never dispatch work items to them (subgoal create rejects it).\n" +
				"After your turn ends the platform pulls the reviewers in\n" +
				"automatically; you never hand work to a reviewer.\n"
		}
		return s
	case "subgoal":
		return "You implement this work item. When the work is DONE, end your turn\n" +
			"with your final message — the platform posts it to the feed as your\n" +
			"report. NEVER post your conclusions with `agentwork goal comment`\n" +
			"and then summarize them again in the final message (the report\n" +
			"double-posts — feed noise). The platform machine-verifies your\n" +
			"branch and produces a Change; the OWNER integrates it. You do not\n" +
			"create sub-goals, hand off, or integrate — those are the owner's\n" +
			"tools.\n"
	case "consult":
		return "You are consulted (READ-ONLY): answer the question in your final\n" +
			"message — the platform posts it to the feed as your answer (do NOT\n" +
			"post a duplicate with `agentwork goal comment`). Do not modify\n" +
			"files, do not commit, do not execute the task itself — your edits\n" +
			"are discarded by the platform.\n"
	case "review":
		return "You REVIEW ONLY — give your opinion, never do the work.\n" +
			"BEFORE you form an opinion, pull the goal's comment feed with\n" +
			"`agentwork goal comments` (use `--after <id>` with the anchor in\n" +
			"your wake line for incremental reads; if you do not remember what\n" +
			"you have seen, pull WITHOUT --after for the full feed). The feed\n" +
			"is your only source of collaboration context: the owner's\n" +
			"completion report, other agents' consult answers, prior review\n" +
			"opinions (including your own from a previous round), and the\n" +
			"handoff history. Without pulling it you are reviewing blind.\n" +
			"Inspect the worktree's changes (diff, tests, quality) alongside\n" +
			"the feed. If the diff is empty, the goal's deliverable lives in\n" +
			"the feed — judge whether the goal was actually fulfilled there,\n" +
			"and say so explicitly; never report a missing diff as the answer\n" +
			"itself. If information is still insufficient after pulling the\n" +
			"feed (e.g. sub-goal or change state unclear), use the `agentwork`\n" +
			"CLI to fetch more before opining — do not guess. End your turn\n" +
			"with your opinion as the final message — the platform posts it to\n" +
			"the feed (the approver reads it there; do NOT post a duplicate\n" +
			"with `agentwork goal comment`). Do not modify files, do not\n" +
			"commit, do not execute the task itself — your edits are discarded\n" +
			"by the platform.\n"
	case "verify":
		return "You are the verifier for a work item: judge it, then issue\n" +
			"`agentwork subgoal verify <id> --verdict passed|rejected [--summary S]\n" +
			"[--evidence E]` ONCE and end your turn. You may RUN tests, but\n" +
			"never modify files or commit.\n"
	}
	return ""
}

// buildWakeLine renders the per-turn wake statement (决策 6-22): ONE
// unified shape for every wake path — "You were mentioned by <who>
// (comment <id>):" + the reason/task. The anchor is the handle the agent
// passes back to `agentwork goal comments --after`. English platform text;
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
