// 模板仓验证器：用与服务层完全相同的严格解析器（parseTemplate）
// 解析 ../agentwork-templates/ 全部登记模板，并在内存库上实际跑一遍两个 apply 路径。
// 用法：go run ./cmd/validate-templates（在仓库根目录；模板仓在同级 ../agentwork-templates）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"gopkg.in/yaml.v3"
)

type localTester struct{}

func (localTester) TestDomainGit(context.Context, string, string, string) *service.DomainGitProbeResult {
	return &service.DomainGitProbeResult{OK: true, BranchExists: true, ResolvedBranch: "main"}
}

func main() {
	root := "../agentwork-templates"
	st, err := store.Open(":memory:")
	if err != nil {
		panic(err)
	}
	defer st.Close()
	bus := events.NewBus()
	settings := service.NewSettingsService(st)

	files := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		b, _ := os.ReadFile(p)
		files[rel] = string(b)
		return nil
	})

	var reg struct {
		Templates []struct {
			ID   string `yaml:"id"`
			Kind string `yaml:"kind"`
			Path string `yaml:"path"`
		} `yaml:"templates"`
	}
	if err := yaml.Unmarshal([]byte(files["registry.yaml"]), &reg); err != nil {
		fmt.Println("FAIL registry.yaml:", err)
		os.Exit(1)
	}
	fmt.Printf("registry: %d template(s)\n", len(reg.Templates))

	cache := map[string]any{
		"fetched_at": "validation",
		"repo":       map[string]string{"git_url": "local", "branch": "main"},
	}
	var entries []map[string]any
	for _, e := range reg.Templates {
		content := files[e.Path]
		var meta struct {
			APIVersion string `yaml:"api_version"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				ID          string `yaml:"id"`
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
				Version     string `yaml:"version"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(content), &meta); err != nil {
			fmt.Printf("FAIL %s: metadata: %v\n", e.Path, err)
			os.Exit(1)
		}
		if meta.Kind != e.Kind || meta.Metadata.ID != e.ID {
			fmt.Printf("FAIL %s: kind/id disagree with registry\n", e.Path)
			os.Exit(1)
		}
		if _, _, err := service.ParseTemplateForTest(content); err != nil {
			fmt.Printf("FAIL %s: strict parse: %v\n", e.Path, err)
			os.Exit(1)
		}
		entries = append(entries, map[string]any{
			"api_version": meta.APIVersion, "kind": meta.Kind, "id": meta.Metadata.ID,
			"name": meta.Metadata.Name, "description": meta.Metadata.Description,
			"version": meta.Metadata.Version, "path": e.Path, "spec_yaml": content,
		})
		fmt.Printf("  OK   %s (%s %s v%s)\n", e.Path, meta.Kind, meta.Metadata.ID, meta.Metadata.Version)
	}
	cache["templates"] = entries
	skillFiles := map[string]string{}
	for path, content := range files {
		if strings.HasPrefix(path, "skills/") {
			skillFiles[path] = content
		}
	}
	cache["skill_files"] = skillFiles
	raw, _ := json.Marshal(cache)
	if err := settings.Set(context.Background(), "platform.template_cache", string(raw)); err != nil {
		panic(err)
	}
	for path := range skillFiles {
		if strings.HasSuffix(path, "SKILL.md") {
			fmt.Println("skill package:", path)
		}
	}

	if _, err := st.DB().Exec(`INSERT INTO runtime (id,name,args,env,status,created_at) VALUES ('rt','test-rt','[]','{}','active','t')`); err != nil {
		panic(err)
	}
	apply := service.NewTemplateApplyService(st, service.NewTemplateService(st, settings),
		service.NewAgentService(st, bus), service.NewSkillService(st),
		service.NewSquadService(st, bus), service.NewDomainService(st, bus),
		service.NewGoalService(st, bus), service.NewScheduleService(st, bus),
		localTester{})
	ctx := context.Background()

	// 1. squad apply with rename
	r1, err := apply.ApplySquad(ctx, "backend-dev", service.ApplySquadOverrides{
		SquadName: "验证-后端小队",
		Rename:    map[string]string{"backend-leader": "验证-负责人"},
	})
	fatalIf("ApplySquad backend-dev", err, r1 == nil || r1.Squad == nil)
	fmt.Printf("ApplySquad backend-dev: OK (squad=%s agents=%d items=%d)\n", r1.Squad.Name, len(r1.Agents), len(r1.Items))

	// 2. create-type project without token → clean repo_token error
	_, err = apply.ApplyProject(ctx, "go-service", service.ApplyProjectOverrides{Name: "验证-新建仓"})
	if err == nil || !strings.Contains(err.Error(), "repo_token") {
		fmt.Println("FAIL: go-service without token should demand repo_token, got:", err)
		os.Exit(1)
	}
	fmt.Println("ApplyProject go-service (no token): clean error OK —", err)

	// 3. existing-repo template without git_url → clean git_url error
	_, err = apply.ApplyProject(ctx, "existing-repo", service.ApplyProjectOverrides{Name: "验证-已有仓"})
	if err == nil || !strings.Contains(err.Error(), "git_url") {
		fmt.Println("FAIL: existing-repo without git_url should demand git_url, got:", err)
		os.Exit(1)
	}
	fmt.Println("ApplyProject existing-repo (no git_url): clean error OK —", err)

	// 4. scratch apply end-to-end (goals started via overlay)
	start := true
	r2, err := apply.ApplyProject(ctx, "weekly-report", service.ApplyProjectOverrides{
		Name: "验证-周报", GoalStart: &start,
	})
	fatalIf("ApplyProject weekly-report", err, r2 == nil || r2.Domain == nil)
	fmt.Printf("ApplyProject weekly-report: OK (domain=%s agents=%d goals=%d schedules=%d items=%d)\n",
		r2.Domain.Name, len(r2.Agents), len(r2.Goals), len(r2.Schedules), len(r2.Items))
	for _, g := range r2.Goals {
		if g.Status != "active" {
			fmt.Println("FAIL: goal_start overlay ignored:", g.Title, g.Status)
			os.Exit(1)
		}
	}
	fmt.Println("ALL TEMPLATE VALIDATIONS PASSED")
}

func fatalIf(name string, err error, cond bool) {
	if err != nil || cond {
		fmt.Printf("FAIL %s: %v\n", name, err)
		os.Exit(1)
	}
}
