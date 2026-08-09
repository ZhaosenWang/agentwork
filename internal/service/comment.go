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
// server parses AFTER persistence to trigger runs. See DESIGN.zh.md §5.3.
type Comment struct {
	ID         string `json:"id"`
	GoalID     string `json:"goal_id"`
	AuthorType string `json:"author_type"` // human|agent|system
	AuthorID   string `json:"author_id"`
	ParentID   string `json:"parent_id"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
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

// MentionRe matches `[@Name](mention://(agent|squad|human|all)/(<uuid-ish>|all))`.
// Matches multica's parser shape (server/internal/util/mention.go): only
// structured Markdown URIs, only UUID-ish ids (or literal "all"). Bare `@handle`
// prose does NOT match — the agent must resolve a UUID and write the link.
// See DESIGN.zh.md §5.3 ("only structured URIs, only UUID").
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
// Per DESIGN.zh.md §5.3:
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
	// Checkpoint freeze (DESIGN.v2.md §4, decision 2-3): while the goal is in
	// review, mentions only land as comments — no run is triggered, so the
	// branch state under the human's decision is never mutated underneath the
	// approval. The pending mention is visible in the goal timeline and can be
	// acted on after the review resolves.
	if HasMentionAll(c.Content) || g.Status == "review" {
		// @all: notify humans only (TBD: no inbox in MVP) and suppress triggers.
		// review: comment lands, triggers suppressed.
		return &c, nil
	}
	for _, m := range ParseMentions(c.Content) {
		switch m.Type {
		case "agent":
			if s.runSvc == nil {
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