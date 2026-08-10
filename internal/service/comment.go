package service

import (
	"context"
	"database/sql"
	"encoding/json"
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
	var out []Mention
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

// Create persists a comment and dispatches agent/squad mentions.
//
// Mention dispatch matrix (agent vs human author):
//
//	Agent @mentions another agent → HANDOFF (reassign goal + system comment + run)
//	Agent @mentions self           → guest run only (coalesce)
//	Human @mentions an agent       → guest run only (ask a question, keep owner)
//	Agent/Human @mentions squad    → leader run (handoff if agent-initiated)
//	Human mentions                 → render link only
//	@all                           → suppress all triggers
func (s *CommentService) Create(ctx context.Context, c Comment) (*Comment, error) {
	if c.GoalID == "" {
		return nil, NewValidationError("goal_id is required")
	}
	if c.AuthorType == "" {
		c.AuthorType = "human"
	}
	if _, err := s.goalSvc.Get(ctx, c.GoalID); err != nil {
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

	if HasMentionAll(c.Content) {
		return &c, nil
	}

	mentions := ParseMentions(c.Content)
	handoffDone := false
	currentGoal, _ := s.goalSvc.Get(ctx, c.GoalID) // best-effort; nil means no handoff possible

	for _, m := range mentions {
		switch m.Type {
		case "agent":
			if s.runSvc == nil {
				continue
			}
			// Agent-initiated mention to a DIFFERENT agent → handoff
			if c.AuthorType == "agent" && !handoffDone && currentGoal != nil &&
				(currentGoal.AssigneeType != "agent" || currentGoal.AssigneeID != m.ID) {
				if err := s.performHandoff(ctx, c, currentGoal, "agent", m.ID); err == nil {
					handoffDone = true
				}
				continue
			}
			// Human-initiated mention, self-mention, or subsequent mention → guest run
			if _, e := s.runSvc.EnqueueForMention(ctx, c.GoalID, m.ID, c.ID); e != nil {
				continue
			}
		case "squad":
			if s.runSvc == nil {
				continue
			}
			if c.AuthorType == "agent" && !handoffDone && currentGoal != nil &&
				(currentGoal.AssigneeType != "squad" || currentGoal.AssigneeID != m.ID) {
				if err := s.performSquadHandoff(ctx, c, currentGoal, m.ID); err == nil {
					handoffDone = true
				}
				continue
			}
			if e := s.enqueueLeaderRunForMention(ctx, m.ID, c.GoalID, c.ID); e != nil {
				continue
			}
		case "human", "all", "issue":
			// No run; just a rendered link.
		}
	}
	return &c, nil
}

// performHandoff reassigns the goal + posts a system comment + enqueues a run.
func (s *CommentService) performHandoff(ctx context.Context, c Comment, current *Goal, newType, newID string) error {
	// 1. Change the goal's assignee
	if _, err := s.goalSvc.Assign(ctx, c.GoalID, newType, newID, ""); err != nil {
		return err
	}
	// 2. Post a system comment recording the handoff
	content := fmt.Sprintf("Handoff: %s/%s → %s/%s (via @mention by %s/%s)",
		current.AssigneeType, current.AssigneeID, newType, newID,
		c.AuthorType, c.AuthorID)
	if err := s.insertSystemComment(ctx, c.GoalID, content); err != nil {
		return err
	}
	// 3. Cancel the handing-off agent's active run so it can't keep working
	//    and accidentally steal the goal back.
	s.bus.Publish(ctx, events.Event{Topic: "handoff:completed", Payload: map[string]any{
		"goal_id":       c.GoalID,
		"from_agent_id": c.AuthorID,
	}})
	// 4. Enqueue a run for the new assignee
	if newType == "agent" {
		_, err := s.runSvc.EnqueueForMention(ctx, c.GoalID, newID, c.ID)
		return err
	}
	return s.enqueueLeaderRunForMention(ctx, c.GoalID, newID, c.ID)
}

// performSquadHandoff is the squad variant of performHandoff.
func (s *CommentService) performSquadHandoff(ctx context.Context, c Comment, current *Goal, squadID string) error {
	if _, err := s.goalSvc.Assign(ctx, c.GoalID, "squad", squadID, ""); err != nil {
		return err
	}
	content := fmt.Sprintf("Handoff: %s/%s → squad/%s (via @mention by %s/%s)",
		current.AssigneeType, current.AssigneeID, squadID,
		c.AuthorType, c.AuthorID)
	if err := s.insertSystemComment(ctx, c.GoalID, content); err != nil {
		return err
	}
	return s.enqueueLeaderRunForMention(ctx, c.GoalID, squadID, c.ID)
}

// insertSystemComment directly inserts a system-authored comment (avoids
// calling Create recursively).
func (s *CommentService) insertSystemComment(ctx context.Context, goalID, content string) error {
	c := Comment{
		ID: newID(), GoalID: goalID, AuthorType: "system", AuthorID: "",
		Content: content, CreatedAt: now(),
	}
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,?,?,?)`,
		c.ID, c.GoalID, c.AuthorType, c.AuthorID, nil, c.Content, c.CreatedAt); err != nil {
		return fmt.Errorf("insert system comment: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), goalID, "system", "", "commented", "{}", c.CreatedAt); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "comment:created", Payload: c})
	return nil
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
	var out []Comment
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
var _ = json.Marshal
