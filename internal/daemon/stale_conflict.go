package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
)

// staleConflictResolver implements service.StaleConflictResolver. It lives in
// the daemon layer because the ancestry check needs the shared bare repo on
// disk — the service layer has no git handle. The resolver is a standalone
// struct (not *Daemon) to avoid a circular dependency between Daemon and
// GoalService.
type staleConflictResolver struct {
	st *store.Store
}

// StaleConflictChanges returns the IDs of conflict changes on goalID whose
// latest revision head_ref is already an ancestor of the goal branch HEAD
// (the content is already on the branch — the conflict is stale). A nil/empty
// result (scratch domain, missing repo, no conflict changes, git error) means
// "nothing to clear"; the caller proceeds without cleanup.
func (r *staleConflictResolver) StaleConflictChanges(ctx context.Context, goalID string) ([]string, error) {
	var domainID, defaultBranch, domainType string
	err := r.st.DB().QueryRowContext(ctx,
		`SELECT d.id, d.default_branch, COALESCE(d.type,'') FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`,
		goalID).Scan(&domainID, &defaultBranch, &domainType)
	if err == sql.ErrNoRows {
		return nil, nil // goal vanished — nothing to resolve
	}
	if err != nil {
		return nil, fmt.Errorf("load goal/domain: %w", err)
	}
	// Scratch domains have no git repo — no ancestry to check.
	if domainType == "scratch" {
		return nil, nil
	}
	repo := domainRepoPath(domainID)
	if _, err := os.Stat(repo); err != nil {
		return nil, nil // repo not on disk — nothing to resolve
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	goalBranch := goalBranchName(goalID)
	// The goal branch HEAD. If the branch doesn't exist yet locally, try the
	// remote-tracking ref; if neither exists, there's nothing to be an ancestor
	// of — return empty.
	goalHead, err := gitRun(ctx, repo, "rev-parse", goalBranch)
	if err != nil {
		goalHead, err = gitRun(ctx, repo, "rev-parse", "origin/"+goalBranch)
		if err != nil {
			return nil, nil // no goal branch — no stale conflicts possible
		}
	}
	goalHead = strings.TrimSpace(goalHead)
	if goalHead == "" {
		return nil, nil
	}

	// Load this goal's conflict changes with their latest revision head_ref.
	rows, err := r.st.DB().QueryContext(ctx,
		`SELECT c.id,
		        COALESCE((SELECT r.head_ref FROM change_revision r WHERE r.change_id = c.id ORDER BY r.seq DESC LIMIT 1), '')
		 FROM change c WHERE c.goal_id=? AND c.status='conflict'`, goalID)
	if err != nil {
		return nil, fmt.Errorf("load conflict changes: %w", err)
	}
	defer rows.Close()
	var staleIDs []string
	for rows.Next() {
		var changeID, headRef string
		if err := rows.Scan(&changeID, &headRef); err != nil {
			return nil, fmt.Errorf("scan conflict change: %w", err)
		}
		if headRef == "" {
			continue
		}
		// Is head_ref already merged into the goal branch? git exits 0 if it is
		// an ancestor, non-zero otherwise.
		if _, err := gitRun(ctx, repo, "merge-base", "--is-ancestor", headRef, goalHead); err == nil {
			staleIDs = append(staleIDs, changeID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflict changes: %w", err)
	}
	if len(staleIDs) > 0 {
		logging.Infof("stale-conflict: goal %s has %d stale conflict change(s)", goalID, len(staleIDs))
	}
	return staleIDs, nil
}
