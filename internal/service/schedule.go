package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Schedule is a cron-triggered goal template. Each firing clones a fresh goal
// from TitleTemplate/Description, assigns it to AssigneeID, and enqueues a
// run. assignee may be an agent or a squad.
type Schedule struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	TitleTemplate  string `json:"title_template"`
	Description    string `json:"description"`
	AssigneeType   string `json:"assignee_type"` // agent | squad
	AssigneeID     string `json:"assignee_id"`
	CronExpression string `json:"cron_expression"`
	Timezone       string `json:"timezone"`
	Enabled        bool   `json:"enabled"`
	NextRunAt      string `json:"next_run_at"`
	LastRunAt      string `json:"last_run_at"`
	CreatedAt      string `json:"created_at"`
}

type ScheduleService struct {
	st  *store.Store
	bus *events.Bus
}

func NewScheduleService(st *store.Store, bus *events.Bus) *ScheduleService {
	return &ScheduleService{st: st, bus: bus}
}

// Create validates the cron expression, computes the first next_run_at, and
// inserts the schedule.
func (s *ScheduleService) Create(ctx context.Context, sch Schedule) (*Schedule, error) {
	if sch.Name == "" {
		return nil, NewValidationError("name is required")
	}
	if sch.TitleTemplate == "" {
		return nil, NewValidationError("title_template is required")
	}
	if sch.AssigneeID == "" {
		return nil, NewValidationError("assignee_id is required")
	}
	if sch.AssigneeType == "" {
		sch.AssigneeType = "agent"
	}
	switch sch.AssigneeType {
	case "agent":
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, sch.AssigneeID, "assignee agent"); err != nil {
			return nil, err
		}
	case "squad":
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, sch.AssigneeID, "assignee squad"); err != nil {
			return nil, err
		}
	default:
		return nil, NewValidationError("assignee_type must be agent or squad")
	}
	if sch.CronExpression == "" {
		return nil, NewValidationError("cron_expression is required")
	}
	if sch.Timezone == "" {
		sch.Timezone = "UTC"
	}
	if err := ValidateCron(sch.CronExpression, sch.Timezone); err != nil {
		return nil, err
	}
	sch.ID = newID()
	sch.CreatedAt = now()
	sch.Enabled = true

	next, err := ComputeNextRun(sch.CronExpression, sch.Timezone, time.Now())
	if err != nil {
		return nil, err
	}
	sch.NextRunAt = next.Format(time.RFC3339Nano)

	enabled := 0
	if sch.Enabled {
		enabled = 1
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO schedule (id,name,title_template,description,assignee_type,assignee_id,cron_expression,timezone,enabled,next_run_at,last_run_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,'')`,
		sch.ID, sch.Name, sch.TitleTemplate, sch.Description, sch.AssigneeType, sch.AssigneeID, sch.CronExpression, sch.Timezone, enabled, sch.NextRunAt, sch.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert schedule: %w", err)
	}

	s.bus.Publish(ctx, events.Event{Topic: "schedule:created", Payload: sch})
	return &sch, nil
}

func (s *ScheduleService) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,title_template,description,assignee_type,assignee_id,cron_expression,timezone,enabled,next_run_at,last_run_at,created_at
		 FROM schedule ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sch Schedule
		var enabled int
		if err := rows.Scan(&sch.ID, &sch.Name, &sch.TitleTemplate, &sch.Description, &sch.AssigneeType, &sch.AssigneeID, &sch.CronExpression, &sch.Timezone, &enabled, &sch.NextRunAt, &sch.LastRunAt, &sch.CreatedAt); err != nil {
			return nil, err
		}
		sch.Enabled = enabled != 0
		out = append(out, sch)
	}
	return out, rows.Err()
}

func (s *ScheduleService) Get(ctx context.Context, id string) (*Schedule, error) {
	var sch Schedule
	var enabled int
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,title_template,description,assignee_type,assignee_id,cron_expression,timezone,enabled,next_run_at,last_run_at,created_at
		 FROM schedule WHERE id=?`, id).
		Scan(&sch.ID, &sch.Name, &sch.TitleTemplate, &sch.Description, &sch.AssigneeType, &sch.AssigneeID, &sch.CronExpression, &sch.Timezone, &enabled, &sch.NextRunAt, &sch.LastRunAt, &sch.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sch.Enabled = enabled != 0
	return &sch, nil
}

func (s *ScheduleService) Delete(ctx context.Context, id string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Drop run history first (FK), then the schedule. Same effect as ON DELETE
	// CASCADE; we keep the FK non-cascading to match the rest of the schema.
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_run WHERE schedule_id=?`, id); err != nil {
		return fmt.Errorf("delete schedule_run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	return tx.Commit()
}
