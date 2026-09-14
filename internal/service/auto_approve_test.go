package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eushing/agentwork/internal/store"
)

func autoApproveStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "aw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func setDigestScheduleMarker(t *testing.T, st *store.Store, scheduleID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,?)`,
		DigestKeySchedule, `"`+scheduleID+`"`, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set digest marker: %v", err)
	}
}

func TestIsAutoApproveGoalHumanGoal(t *testing.T) {
	st := autoApproveStore(t)
	if IsAutoApproveGoal(context.Background(), st.DB(), "human", "") {
		t.Fatal("human goal must not be auto-approve")
	}
}

func TestIsAutoApproveGoalImportGoal(t *testing.T) {
	st := autoApproveStore(t)
	if !IsAutoApproveGoal(context.Background(), st.DB(), "system", ImportCreatedByID) {
		t.Fatal("team_import goal must be auto-approve")
	}
}

func TestIsAutoApproveGoalDigestGoal(t *testing.T) {
	st := autoApproveStore(t)
	const schedID = "sched-abc-123"
	setDigestScheduleMarker(t, st, schedID)
	if !IsAutoApproveGoal(context.Background(), st.DB(), "system", schedID) {
		t.Fatal("digest goal must be auto-approve")
	}
}

func TestIsAutoApproveGoalSystemGoalWrongID(t *testing.T) {
	st := autoApproveStore(t)
	setDigestScheduleMarker(t, st, "sched-abc-123")
	if IsAutoApproveGoal(context.Background(), st.DB(), "system", "some-other-id") {
		t.Fatal("system goal with unknown created_by_id must not be auto-approve")
	}
}

func TestIsAutoApproveGoalDigestMarkerMissing(t *testing.T) {
	st := autoApproveStore(t)
	if IsAutoApproveGoal(context.Background(), st.DB(), "system", "sched-abc-123") {
		t.Fatal("digest goal without app_settings marker must not be auto-approve")
	}
}

func TestIsAutoApproveGoalEmptyCreatedByID(t *testing.T) {
	st := autoApproveStore(t)
	if IsAutoApproveGoal(context.Background(), st.DB(), "system", "") {
		t.Fatal("system goal with empty created_by_id must not be auto-approve")
	}
}
