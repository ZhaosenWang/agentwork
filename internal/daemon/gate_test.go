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
// fire on the run's changed paths, request never fires here (it is set
// directly by RequestApproval).
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
	if err := commitRunChanges(ctx, dir, "x"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	checks := service.Checks{
		Gates: []service.GateRule{
			{Name: "merge", When: "每次完成都需审批"},
			{Name: "diff_contains", When: "动 config 必审", Pattern: "config/*"},
			{Name: "diff_excludes", When: "不许碰密钥", Pattern: "*.secret"},
			{Name: "request", When: "agent 请求"},
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
	if strings.Contains(joined, "diff_excludes") || strings.Contains(joined, "request") {
		t.Fatalf("unrelated gates must not fire: %s", joined)
	}

	// No gates → nothing fires.
	if hit := evalGates(ctx, dir, base, service.Checks{}); len(hit) != 0 {
		t.Fatalf("empty gates must fire nothing: %v", hit)
	}
}
