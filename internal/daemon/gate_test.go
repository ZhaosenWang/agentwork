package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/service"
)

// TestEvalGates covers the M2 gate rule engine: merge always fires, diff_*
// fire on the run's changed paths, unknown gate kinds never fire.
func TestEvalGates(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()
	base, _ := gitRun(ctx, dir, "rev-parse", "HEAD")

	// Touch config/prod.yaml (committed) → diff_contains config/* + merge fire.
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "prod.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
		t.Fatalf("commit: %v", err)
	}

	checks := service.Checks{
		Gates: []service.GateRule{
			{Name: "merge", When: "每次完成都需审批"},
			{Name: "diff_contains", When: "动 config 必审", Pattern: "config/*"},
			{Name: "diff_excludes", When: "不许碰密钥", Pattern: "*.secret"},
			{Name: "bogus", When: "不存在的卡点"},
		},
	}
	hit := evalGates(ctx, dir, base, checks)
	joined := strings.Join(hit, "; ")
	if !strings.Contains(joined, "merge") {
		t.Fatalf("merge gate must always fire: %s", joined)
	}
	if !strings.Contains(joined, "diff_contains") {
		t.Fatalf("diff_contains config/* must fire on config/prod.yaml: %s", joined)
	}
	if strings.Contains(joined, "diff_excludes") || strings.Contains(joined, "bogus") {
		t.Fatalf("unrelated gates must not fire: %s", joined)
	}

	// No gates → nothing fires (no deletions in this repo).
	if hit := evalGates(ctx, dir, base, service.Checks{}); len(hit) != 0 {
		t.Fatalf("empty gates must fire nothing: %v", hit)
	}
}

// TestEvalGatesBuiltinDeleteBaseline: the platform security baseline fires
// on any DELETED file regardless of domain policy — deleting is judged by a
// human, never unattended (DESIGN.md §5.3, built-in and not overridable).
func TestEvalGatesBuiltinDeleteBaseline(t *testing.T) {
	dir := newTestRepo(t)
	ctx := context.Background()

	// A file that EXISTS at the baseline is deleted by the run.
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	base2, _ := gitRun(ctx, dir, "rev-parse", "HEAD")
	if err := os.Remove(keep); err != nil {
		t.Fatal(err)
	}
	if err := commitRunChanges(ctx, dir, "x", nil); err != nil {
		t.Fatalf("commit delete: %v", err)
	}

	hit := evalGates(ctx, dir, base2, service.Checks{}) // EMPTY domain policy
	joined := strings.Join(hit, "; ")
	if !strings.Contains(joined, "删除文件必审") || !strings.Contains(joined, "keep.txt") {
		t.Fatalf("built-in delete baseline must fire on deleted files: %s", joined)
	}
}
