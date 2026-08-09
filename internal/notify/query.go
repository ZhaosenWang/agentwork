package notify

import (
	"context"

	"github.com/eushing/agentwork/internal/store"
)

// The notify layer's read-only store access (M3). Before M3, notify was a
// pure event fan-out: every milestone message was fully described by the bus
// event payload. The approval card needs the run's evidence bundle (not in
// the event), the daily digest aggregates goals (no event at all), and the
// intake flow needs the platform roster to hand the parser agent. The
// interface stays minimal and read-only — notify never writes.

// ReviewGoal is one goal parked in review plus the run whose evidence the
// approval card shows (the latest completed run of the goal).
type ReviewGoal struct {
	GoalID   string
	Title    string
	Reason   string
	RunID    string // the evidence run — recorded on the gate_decision (audit chain)
	Evidence string // the run.evidence JSON bundle
}

// GoalBrief is one goal in a digest aggregation.
type GoalBrief struct {
	GoalID string
	Title  string
	Status string
}

// GoalStatusView is the intake "goal status" answer: the goal plus its last
// run's outcome summary.
type GoalStatusView struct {
	GoalID        string
	Title         string
	Status        string
	ReviewRequest string
	Summary       string
}

// NamedID is an id + name pair (agent/domain roster rows).
type NamedID struct {
	ID   string
	Name string
}

// QueryStore is the read-only store surface the notify layer consumes.
type QueryStore interface {
	// ReviewGoals lists goals parked in review with their evidence run.
	ReviewGoals(ctx context.Context) ([]ReviewGoal, error)
	// GoalTitle resolves one goal's title (milestone cards carry it).
	GoalTitle(ctx context.Context, goalID string) (string, error)
	// GoalStatus resolves a goal by id OR id prefix (intake "状态 <id>"
	// queries accept the short id).
	GoalStatus(ctx context.Context, idPrefix string) (*GoalStatusView, error)
	// Agents/Domains are the roster handed to the intake parser prompt so it
	// can resolve assignee/domain names to ids.
	Agents(ctx context.Context) ([]NamedID, error)
	Domains(ctx context.Context) ([]NamedID, error)
	// TerminalSince lists goals whose LAST run reached a terminal status at or
	// after since (RFC3339) — the daily digest's "yesterday" aggregation.
	TerminalSince(ctx context.Context, since string) ([]GoalBrief, error)
}

// SQLQueryStore is the production QueryStore over SQLite. Single-user: every
// query targets the one DB connection pool.
type SQLQueryStore struct {
	st *store.Store
}

func NewSQLQueryStore(st *store.Store) *SQLQueryStore {
	return &SQLQueryStore{st: st}
}

func (q *SQLQueryStore) ReviewGoals(ctx context.Context) ([]ReviewGoal, error) {
	rows, err := q.st.DB().QueryContext(ctx,
		`SELECT g.id, g.title, g.review_request,
		        COALESCE((SELECT r.id FROM run r WHERE r.goal_id=g.id AND r.status='completed'
		                  ORDER BY r.finished_at DESC LIMIT 1), ''),
		        COALESCE((SELECT r.evidence FROM run r WHERE r.goal_id=g.id AND r.status='completed'
		                  ORDER BY r.finished_at DESC LIMIT 1), '')
		 FROM goal g WHERE g.status='review' ORDER BY g.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReviewGoal{}
	for rows.Next() {
		var r ReviewGoal
		if err := rows.Scan(&r.GoalID, &r.Title, &r.Reason, &r.RunID, &r.Evidence); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (q *SQLQueryStore) GoalTitle(ctx context.Context, goalID string) (string, error) {
	var t string
	err := q.st.DB().QueryRowContext(ctx, `SELECT title FROM goal WHERE id=?`, goalID).Scan(&t)
	return t, err
}

func (q *SQLQueryStore) GoalStatus(ctx context.Context, idPrefix string) (*GoalStatusView, error) {
	var v GoalStatusView
	err := q.st.DB().QueryRowContext(ctx,
		`SELECT g.id, g.title, g.status, g.review_request,
		        COALESCE((SELECT r.result_summary FROM run r WHERE r.goal_id=g.id
		                  AND r.status IN ('completed','failed') ORDER BY r.finished_at DESC LIMIT 1), '')
		 FROM goal g WHERE g.id LIKE ? || '%' ORDER BY g.created_at DESC LIMIT 1`, idPrefix).
		Scan(&v.GoalID, &v.Title, &v.Status, &v.ReviewRequest, &v.Summary)
	return &v, err
}

func (q *SQLQueryStore) Agents(ctx context.Context) ([]NamedID, error) {
	rows, err := q.st.DB().QueryContext(ctx, `SELECT id, name FROM agent ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NamedID{}
	for rows.Next() {
		var n NamedID
		if err := rows.Scan(&n.ID, &n.Name); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (q *SQLQueryStore) Domains(ctx context.Context) ([]NamedID, error) {
	rows, err := q.st.DB().QueryContext(ctx, `SELECT id, name FROM domain ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NamedID{}
	for rows.Next() {
		var n NamedID
		if err := rows.Scan(&n.ID, &n.Name); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (q *SQLQueryStore) TerminalSince(ctx context.Context, since string) ([]GoalBrief, error) {
	rows, err := q.st.DB().QueryContext(ctx,
		`SELECT g.id, g.title, g.status FROM goal g
		 WHERE g.status IN ('done','failed')
		   AND EXISTS (SELECT 1 FROM run r WHERE r.goal_id=g.id
		               AND r.status IN ('completed','failed','cancelled') AND r.finished_at >= ?)`,
		since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GoalBrief{}
	for rows.Next() {
		var b GoalBrief
		if err := rows.Scan(&b.GoalID, &b.Title, &b.Status); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

var _ QueryStore = (*SQLQueryStore)(nil)
