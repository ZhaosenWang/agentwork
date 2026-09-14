package service

import (
	"context"
	"database/sql"
	"strings"
)

// rowQueryer is the minimal read surface both *sql.DB and *sql.Tx satisfy —
// IsAutoApproveGoal reads app_settings inside the goal reconcile's tx or the
// daemon's bare store with the same call.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// IsAutoApproveGoal reports whether the goal's review checkpoint is
// auto-approved by the system (digest + team import) and therefore never
// reaches a human. Callers that already loaded the goal's created_by fields
// pass them in directly to avoid a redundant query.
//
// Import goals carry created_by_id = ImportCreatedByID ("team_import").
// Digest goals carry created_by_id = the built-in schedule's id, stored in
// app_settings under DigestKeySchedule (JSON-quoted). Any other system goal
// is NOT auto-approve — a human still decides.
func IsAutoApproveGoal(ctx context.Context, q rowQueryer, createdByType, createdByID string) bool {
	if createdByType != "system" || createdByID == "" {
		return false
	}
	if createdByID == ImportCreatedByID {
		return true
	}
	var raw string
	if err := q.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key=?`, DigestKeySchedule).Scan(&raw); err != nil {
		return false
	}
	schedID := strings.Trim(raw, `"`)
	return schedID != "" && schedID == createdByID
}
