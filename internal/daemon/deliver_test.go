package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// TestDeliverMergeIdentityNoGlobalGitConfig reproduces the live failure: a
// container with NO global user.name/user.email approves a goal, and the
// deliver step's `git merge --no-ff` used to abort with "Committer identity
// unknown" (misreported as "merge conflict"). The fix passes -c user.name /
// -c user.email to the merge. This test drives the REAL deliverGoal against
// a local bare remote with a feat branch carrying one commit and asserts the
// merge commit lands on the default branch — without any global git identity.
func TestDeliverMergeIdentityNoGlobalGitConfig(t *testing.T) {
	// Isolate HOME so runsRoot() and any ~/.gitconfig are ours alone, and
	// CRUCIALLY so we do not inherit the developer's global user.name/email
	// (the bug only surfaces when git cannot auto-detect an identity).
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Belt-and-suspenders: git still reads /etc/gitconfig. Wipe identity from
	// the test's git invocations by setting GIT_CONFIG_NOSYSTEM and an empty
	// config-global path — the merge MUST work via -c regardless.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig-noop"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── Remote: a bare repo with one commit on main, plus a feat branch
	//    carrying a second commit (the "agent's work" to be merged). ──
	work := t.TempDir()
	seed := filepath.Join(work, "seed")
	mustRunGit(t, "", "init", "-b", "main", seed)
	mustRunGit(t, seed, "config", "user.email", "seed@local")
	mustRunGit(t, seed, "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "add", ".")
	mustRunGit(t, seed, "commit", "-m", "init")

	// feat branch with a real change (diverges from main → a merge commit).
	mustRunGit(t, seed, "checkout", "-b", "feat-test0001")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "add", ".")
	mustRunGit(t, seed, "commit", "-m", "feat: add feature.txt")

	remote := filepath.Join(work, "remote.git")
	mustRunGit(t, "", "clone", "--bare", seed, remote)

	domainID := "dom-deliver-identity"
	goalID := "test-goal-0001"
	// The bare clone mirrors all branches into refs/heads/*; deliver's
	// ensureSharedRepo re-points remote.origin.fetch to refs/remotes/origin/*.
	// Rename the feat branch to the EXACT name deliver will look for
	// (goalBranchName truncates goalID to 8 chars).
	shortID := goalID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	deliverBranch := "feat-" + shortID
	mustRunGit(t, remote, "branch", "-m", "feat-test0001", deliverBranch)

	// ── Daemon + store with a domain pointing at the bare remote, and a goal
	//    in review whose branch is feat-test0001. ──
	dbPath := filepath.Join(work, "aw.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc}

	ts := time.Now().Format(time.RFC3339)
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO domain (id,type,name,git_url,default_branch,git_identity,git_credentials,created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		domainID, "repo", "deliver-test", "file://"+remote, "main", "", "", ts); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO goal (id,title,domain_id,assignee_type,assignee_id,status,created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		goalID, "deliver identity test", domainID, "agent", "dummy", "review", ts); err != nil {
		t.Fatalf("insert goal: %v", err)
	}

	// ── Drive deliver. onGoalApproved launches a goroutine; call deliverGoal
	//    directly and synchronously. ──
	d.deliverGoal(ctx, goalID)

	// ── Assert: goal advanced to done (MarkDelivered on merge success). ──
	var status string
	if err := st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, goalID).Scan(&status); err != nil {
		t.Fatalf("read goal: %v", err)
	}
	if status != "done" {
		var note string
		_ = st.DB().QueryRowContext(ctx, `SELECT review_request FROM goal WHERE id=?`, goalID).Scan(&note)
		t.Fatalf("goal status = %q, want done. review_request=%q", status, note)
	}

	// ── Assert: the merge commit is on main of the remote (a --no-ff merge
	//    produces a commit with TWO parents). This is the operation that used
	//    to fail with "Committer identity unknown". ──
	out, err := exec.Command("git", "-C", remote, "log", "--format=%P", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("git log main on remote: %v: %s", err, out)
	}
	mergeFound := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" && len(strings.Fields(line)) == 2 {
			mergeFound = true
			break
		}
	}
	if !mergeFound {
		t.Fatalf("no merge commit (2-parent) found on main; log:\n%s", out)
	}
}
