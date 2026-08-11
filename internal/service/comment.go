package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Comment is a message under a goal. Authors are polymorphic (human/agent/
// system). content is Markdown and may carry structured mention URIs that the
// server parses AFTER persistence to trigger runs. See DESIGN.md §2.
type Comment struct {
	ID         string `json:"id"`
	GoalID     string `json:"goal_id"`
	AuthorType string `json:"author_type"` // human|agent|system
	AuthorID   string `json:"author_id"`
	ParentID   string `json:"parent_id"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
	// RunID is a CREATE-ONLY request field: an agent's comment carries the
	// run it was made in (AGENTWORK_RUN_ID), and the platform threads it as
	// a REPLY to that run's trigger comment (the mention that started it) —
	// collaboration conversations chain automatically, no agent cooperation.
	RunID string `json:"run_id,omitempty"`
}

type CommentService struct {
	st      *store.Store
	bus     *events.Bus
	runSvc  *RunService
	goalSvc *GoalService
}

func NewCommentService(st *store.Store, bus *events.Bus) *CommentService {
	return &CommentService{st: st, bus: bus}
}

func (s *CommentService) SetRunService(rs *RunService)  { s.runSvc = rs }
func (s *CommentService) SetGoalService(gs *GoalService) { s.goalSvc = gs }

// Mention is one parsed mention from a comment body.
type Mention struct {
	Type string `json:"type"` // agent | squad | human | all
	ID   string `json:"id"`
}

// Mention-cycle guard (two thresholds): a goal whose runs are repeatedly
// triggered by AGENT comments is likely an agent ping-pong (A mentions B,
// B mentions A, ...). Healthy work does not churn agent-triggered runs;
// repeated churn (including repeated human rejects) means the task itself is
// stuck. Over maxMentionHints the next run's prompt warns the agents to stop
// circular handoffs; over maxMentionCycle the next trigger is refused and
// the goal fails with the cycle count as the reason.
const (
	MaxMentionHints  = 4
	MaxMentionCycle  = 8
)

// MentionRe matches `[@Name](mention://(agent|squad|human|all)/(<uuid-ish>|all))`.
// Matches multica's parser shape (server/internal/util/mention.go): only
// structured Markdown URIs, only UUID-ish ids (or literal "all"). Bare `@handle`
// prose does NOT match — the agent must resolve a UUID and write the link.
// See DESIGN.md §2 ("only structured URIs, only UUID").
var MentionRe = regexp.MustCompile(`\[@?(.+?)\]\(mention://(agent|squad|human|all)/([0-9a-fA-F-]+|all)\)`)

// ParseMentions extracts deduplicated mentions from persisted comment body.
// NEVER called on agent stdout — only on already-stored comment content.
func ParseMentions(content string) []Mention {
	matches := MentionRe.FindAllStringSubmatch(content, -1)
	seen := map[string]struct{}{}
	out := []Mention{}
	for _, m := range matches {
		mention := Mention{Type: m[2], ID: m[3]}
		key := mention.Type + ":" + mention.ID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, mention)
	}
	return out
}

// HasMentionAll reports whether the content mentions @all.
func HasMentionAll(content string) bool {
	for _, m := range ParseMentions(content) {
		if m.Type == "all" {
			return true
		}
	}
	return false
}

// Create persists a comment and dispatches any agent/squad mentions it carries.
// Per DESIGN.md:
//   - @all → suppress auto-trigger (no run enqueued); humans notified later.
//   - mention://agent/<id> → enqueue a new run on that agent (same goal,
//     different agent), does NOT cancel the current assignee's in-flight run.
//   - mention://squad/<id> → route to the squad's leader (leader run).
//   - mention://human/<id> → just renders a link.
func (s *CommentService) Create(ctx context.Context, c Comment) (*Comment, error) {
	if c.GoalID == "" {
		return nil, NewValidationError("goal_id is required")
	}
	if c.AuthorType == "" {
		c.AuthorType = "human"
	}
	// Platform threading: an agent comment made inside a run automatically
	// replies to the comment that triggered that run (mention → run →
	// reply). The agent never needs to know parent ids.
	if c.RunID != "" && c.ParentID == "" {
		var tid string
		if err := s.st.DB().QueryRowContext(ctx,
			`SELECT trigger_comment_id FROM run WHERE id=?`, c.RunID).Scan(&tid); err == nil && tid != "" {
			c.ParentID = tid
		}
	}
	g, err := s.goalSvc.Get(ctx, c.GoalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, NewValidationError("goal does not exist")
		}
		return nil, err
	}
	c.ID = newID()
	c.CreatedAt = now()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var parentID any
	if c.ParentID != "" {
		parentID = c.ParentID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,?,?,?)`,
		c.ID, c.GoalID, c.AuthorType, c.AuthorID, parentID, c.Content, c.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert comment: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'commented',?,?)`,
		newID(), c.GoalID, c.AuthorType, c.AuthorID, "{}", c.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.bus.Publish(ctx, events.Event{Topic: "comment:created", Payload: c})

	// Dispatch mentions AFTER the comment is durably stored. @all suppresses
	// auto-trigger entirely (no runs); other mentions enqueue runs.
	// State freeze (DESIGN.md §4, decision 2-3): mentions only trigger on
	// an ACTIVE goal. While the goal is in review (or blocked), a mention
	// lands as a comment — no run — so the branch state under the human's
	// decision is never mutated underneath the approval. The comment stays in
	// the feed (never lost); the human decides whether to act on it.
	//
	// COMMENT-TRIGGERED REOPEN (GitHub's reopen-and-comment): a HUMAN comment
	// on a TERMINAL goal (done/failed/cancelled) that carries an action
	// mention (agent/squad) reopens the goal — "this task is not over" — and
	// the mention then triggers normally. A plain comment without a mention
	// lands only (terminal goals take no silent new work; a stray remark must
	// not burn a run).
	isTerminal := g.Status == "done" || g.Status == "failed" || g.Status == "cancelled"
	hasActionMention := false
	for _, m := range ParseMentions(c.Content) {
		if m.Type == "agent" || m.Type == "squad" {
			hasActionMention = true
			break
		}
	}
	if isTerminal && c.AuthorType == "human" && hasActionMention {
		if _, err := s.goalSvc.Reopen(ctx, c.GoalID, "评论触发重开："+c.Content); err == nil {
			// Reopened → the goal is active now; the mention dispatch below
			// proceeds against the fresh state.
			if g2, err := s.goalSvc.Get(ctx, c.GoalID); err == nil {
				g = g2
			}
		}
	}
	if HasMentionAll(c.Content) || g.Status != "active" {
		// @all: notify humans only (TBD: no inbox in MVP) and suppress triggers.
		// non-active: comment lands, triggers suppressed.
		return &c, nil
	}
	for _, m := range ParseMentions(c.Content) {
		switch m.Type {
		case "agent":
			if s.runSvc == nil {
				continue
			}
			// Mention-cycle guard: an agent-triggered run churn above the hard
			// threshold fails the goal (the task is stuck in a handoff loop).
			if exceeds, err := s.mentionCycleExceeds(ctx, c.GoalID, MaxMentionCycle); err == nil && exceeds {
				s.forceMentionCycleFailed(ctx, c.GoalID)
				continue
			}
			if _, e := s.runSvc.EnqueueForMention(ctx, c.GoalID, m.ID, c.ID); e != nil {
				// A bad/unknown agent UUID → drop, don't fail the whole comment.
				continue
			}
		case "squad":
			if s.runSvc == nil {
				continue
			}
			if exceeds, err := s.mentionCycleExceeds(ctx, c.GoalID, MaxMentionCycle); err == nil && exceeds {
				s.forceMentionCycleFailed(ctx, c.GoalID)
				continue
			}
			// Route to the squad's leader as a leader run.
			if e := s.enqueueLeaderRunForMention(ctx, m.ID, c.GoalID, c.ID); e != nil {
				continue
			}
		case "human", "all", "issue":
			// No run; just a rendered link.
		}
	}
	return &c, nil
}

// MentionCycleCount counts a goal's agent-triggered runs (trigger_comment_id
// pointing at an AGENT-authored comment). Platform triggers (system review
// requests) and human triggers are not agent churn.
func (s *CommentService) MentionCycleCount(ctx context.Context, goalID string) (int, error) {
	var n int
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run r JOIN comment c ON c.id = r.trigger_comment_id
		 WHERE r.goal_id=? AND c.author_type='agent'`, goalID).Scan(&n)
	return n, err
}

// mentionCycleExceeds reports whether the goal's agent-triggered churn is at
// or above the hard threshold (the NEXT trigger is the one refused).
func (s *CommentService) mentionCycleExceeds(ctx context.Context, goalID string, limit int) (bool, error) {
	n, err := s.MentionCycleCount(ctx, goalID)
	if err != nil {
		return false, err
	}
	return n >= limit, nil
}

// forceMentionCycleFailed fails a goal stuck in an agent handoff loop: the
// goal goes failed (the human take-over path, Reopen), the failure reason
// names the cycle count, queued runs are dropped, and the failure is
// recorded in the feed.
func (s *CommentService) forceMentionCycleFailed(ctx context.Context, goalID string) {
	n, err := s.MentionCycleCount(ctx, goalID)
	if err != nil {
		n = MaxMentionCycle
	}
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='failed' WHERE id=? AND status='active'`, goalID)
	if err != nil {
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return // already moved (review/reject raced) — the loop is moot
	}
	reason := fmt.Sprintf("agent 协作循环 %d 次（超过上限 %d）", n, MaxMentionCycle)
	ts := now()
	_, _ = s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,'','mention_cycle_failed',?,?)`,
		newID(), goalID, "system", `{"reason":"`+reason+`"}`, ts)
	_, _ = s.st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
		newID(), goalID, "任务失败："+reason+"。agent 反复互相转移任务，请检查后重开。", ts)
	_, _ = s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled' WHERE goal_id=? AND status='queued'`, goalID)
	s.bus.Publish(ctx, events.Event{Topic: "goal:finished", Payload: map[string]any{
		"goal_id": goalID, "status": "failed", "summary": reason,
	}})
}

// enqueueLeaderRunForMention resolves a squad's leader and enqueues a leader
// run on it for the goal, sourced from the triggering comment.
func (s *CommentService) enqueueLeaderRunForMention(ctx context.Context, squadID, goalID, triggerCommentID string) error {
	var leaderID string
	err := s.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, squadID).Scan(&leaderID)
	if errors.Is(err, sql.ErrNoRows) {
		return NewValidationError("squad not found")
	}
	if err != nil {
		return err
	}
	// Leader runs via mention: enqueue on the leader agent with isLeader + squadID.
	// We reuse enqueue directly (its own tx) — mention-triggered runs may run
	// concurrently with the current assignee's run, which is intended.
	_, err = s.runSvc.enqueue(ctx, goalID, leaderID, 1, true, squadID, triggerCommentID)
	return err
}

func (s *CommentService) List(ctx context.Context, goalID string) ([]Comment, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,goal_id,author_type,author_id,parent_id,content,created_at FROM comment WHERE goal_id=? ORDER BY created_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		var parentID sql.NullString
		if err := rows.Scan(&c.ID, &c.GoalID, &c.AuthorType, &c.AuthorID, &parentID, &c.Content, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.ParentID = parentID.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// sanity check that the agent/squad we mention exists is intentionally NOT done
// here for agent: a stale agent UUID is dropped silently (matching multica's
// blockTarget behavior). Squad is checked in enqueueLeaderRunForMention.