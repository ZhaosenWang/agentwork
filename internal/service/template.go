package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/gitutil"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
	"gopkg.in/yaml.v3"
)

// Template settings keys (app_settings). platform.template_repo is the
// user-overridable source ({git_url, branch}); platform.template_cache holds
// the fetched templates JSON so GET /templates answers from local state even
// when GitCode is unreachable (fetched_at drives staleness).
const (
	templateRepoKey  = "platform.template_repo"
	templateCacheKey = "platform.template_cache"
	// defaultTemplateRepo is the built-in official template repository —
	// used when platform.template_repo is unset.
	defaultTemplateRepo = "https://gitcode.com/Wing-Jason/agentwork-templates.git"
)

// TemplateRepoConfig is the platform.template_repo value.
type TemplateRepoConfig struct {
	GitURL string `json:"git_url"`
	Branch string `json:"branch"`
}

// TemplateMeta is the common front matter of every template (api_version +
// metadata) and the registry index entry shape.
type TemplateMeta struct {
	APIVersion  string   `yaml:"api_version" json:"api_version"`
	Kind        string   `yaml:"kind"        json:"kind"` // squad-template | project-template
	ID          string   `yaml:"id"          json:"id"`
	Name        string   `yaml:"name"        json:"name"`
	Description string   `yaml:"description" json:"description"`
	Version     string   `yaml:"version"     json:"version"`
	Tags        []string `yaml:"tags"        json:"tags,omitempty"`
	Icon        string   `yaml:"icon"        json:"icon,omitempty"`
}

// TemplateSummary is the list-item shape (metadata only, no spec).
type TemplateSummary struct {
	TemplateMeta
	Path string `json:"path"`
}

// TemplateDetail is the get-one shape: metadata + raw YAML + decoded spec.
type TemplateDetail struct {
	TemplateMeta
	Path     string          `json:"path"`
	SpecYAML string          `json:"spec_yaml"`
	Spec     json.RawMessage `json:"spec"` // spec re-encoded as JSON for direct frontend consumption
}

// templateRegistry is the registry.yaml contract.
type templateRegistry struct {
	Templates []struct {
		ID          string   `yaml:"id"`
		Kind        string   `yaml:"kind"`
		Path        string   `yaml:"path"`
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Version     string   `yaml:"version"`
		Tags        []string `yaml:"tags"`
	} `yaml:"templates"`
}

// templateCache is the platform.template_cache value (the fetched snapshot).
type templateCache struct {
	FetchedAt string             `json:"fetched_at"`
	Repo      TemplateRepoConfig `json:"repo"`
	Templates []cachedTemplate   `json:"templates"`
	Errors    []string           `json:"errors,omitempty"`
	// SkillFiles are the embedded skill packages (skills/<name>/<file>:
	// content) captured at fetch time — ApplySquad hands them to
	// SkillService.Create without a second GitCode round-trip.
	SkillFiles map[string]string `json:"skill_files,omitempty"`
}

// cachedTemplate is one fetched template file (raw YAML kept for preview).
type cachedTemplate struct {
	TemplateMeta
	Path     string `json:"path"`
	SpecYAML string `json:"spec_yaml"`
}

// TemplateFetchResult reports one refresh.
type TemplateFetchResult struct {
	Fetched   int      `json:"fetched"`
	Errors    []string `json:"errors,omitempty"`
	FetchedAt string   `json:"fetched_at"`
}

// templateIDRe constrains metadata.id — it doubles as a URL path segment.
var templateIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)

// Template kinds.
const (
	TemplateKindSquad   = "squad-template"
	TemplateKindProject = "project-template"
)

// TemplateService owns the template source lifecycle: fetching the GitCode
// template repository (a shallow clone — the platform already depends on git,
// avoiding a bet on the largely undocumented contents API), validating each
// template's YAML, and caching the snapshot in app_settings.
type TemplateService struct {
	st       *store.Store
	settings *SettingsService
}

func NewTemplateService(st *store.Store, settings *SettingsService) *TemplateService {
	return &TemplateService{st: st, settings: settings}
}

// RepoConfig returns the configured template repo, falling back to the
// built-in default when unset.
func (s *TemplateService) RepoConfig(ctx context.Context) (TemplateRepoConfig, error) {
	var cfg TemplateRepoConfig
	raw, err := s.settings.Get(ctx, templateRepoKey)
	if err != nil || raw == "" {
		return TemplateRepoConfig{GitURL: defaultTemplateRepo, Branch: "main"}, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return TemplateRepoConfig{}, NewValidationError("platform.template_repo is not valid JSON: " + err.Error())
	}
	if strings.TrimSpace(cfg.GitURL) == "" {
		return TemplateRepoConfig{}, NewValidationError("platform.template_repo.git_url is required")
	}
	if strings.TrimSpace(cfg.Branch) == "" {
		cfg.Branch = "main"
	}
	return cfg, nil
}

// SetRepoConfig stores the template repo override (the settings PUT path).
func (s *TemplateService) SetRepoConfig(ctx context.Context, cfg TemplateRepoConfig) error {
	if strings.TrimSpace(cfg.GitURL) == "" {
		return NewFieldRequiredError("git_url")
	}
	if strings.TrimSpace(cfg.Branch) == "" {
		cfg.Branch = "main"
	}
	raw, _ := json.Marshal(cfg)
	return s.settings.Set(ctx, templateRepoKey, string(raw))
}

// Refresh re-fetches the template repository: shallow-clone to a temp dir,
// parse registry.yaml, load every listed template file, validate, and store
// the snapshot in app_settings. On fetch failure the error is returned AND
// the previous cache is kept (the list endpoint degrades gracefully).
func (s *TemplateService) Refresh(ctx context.Context, token string) (*TemplateFetchResult, error) {
	cfg, err := s.RepoConfig(ctx)
	if err != nil {
		return nil, err
	}
	files, err := fetchTemplateFiles(ctx, cfg, token)
	if err != nil {
		return nil, NewCodedErrorDetail(CodeTemplateFetchFailed,
			fmt.Sprintf("拉取模板仓库失败：%v", err),
			map[string]any{"repo": gitutil.SanitizeURL(cfg.GitURL)})
	}
	cache, ferr := parseTemplateFiles(files)
	if ferr != nil {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, ferr.Error(), nil)
	}
	cache.FetchedAt = now()
	cache.Repo = cfg
	// Keep embedded skill packages on the snapshot (skills/<name>/<file>).
	for path, content := range files {
		if strings.HasPrefix(path, "skills/") {
			cache.SkillFiles[path] = content
		}
	}
	raw, _ := json.Marshal(cache)
	if err := s.settings.Set(ctx, templateCacheKey, string(raw)); err != nil {
		return nil, fmt.Errorf("store template cache: %w", err)
	}
	logging.Infof("templates: refreshed from %s — %d template(s), %d error(s)",
		gitutil.SanitizeURL(cfg.GitURL), len(cache.Templates), len(cache.Errors))
	return &TemplateFetchResult{Fetched: len(cache.Templates), Errors: cache.Errors, FetchedAt: cache.FetchedAt}, nil
}

// fetchTemplateFiles shallow-clones the repo and returns {relative path: content}
// for registry.yaml and every template/skill file it references. A token,
// when given, is embedded via gitCloneURL and never logged.
func fetchTemplateFiles(ctx context.Context, cfg TemplateRepoConfig, token string) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "agentwork-templates-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	defer os.RemoveAll(dir)
	url := gitutil.CloneURL(cfg.GitURL, token)
	args := []string{"clone", "--depth", "1"}
	if cfg.Branch != "" {
		args = append(args, "--branch", cfg.Branch, "--single-branch")
	}
	args = append(args, url, dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone: %v: %s", err, strings.TrimSpace(gitutil.SanitizeURL(strings.TrimSpace(string(out)))))
	}
	files := map[string]string{}
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	regRaw, err := read("registry.yaml")
	if err != nil {
		return nil, fmt.Errorf("registry.yaml missing at repo root: %w", err)
	}
	files["registry.yaml"] = regRaw
	var reg templateRegistry
	if err := yaml.Unmarshal([]byte(regRaw), &reg); err != nil {
		return nil, fmt.Errorf("parse registry.yaml: %w", err)
	}
	if len(reg.Templates) == 0 {
		return nil, fmt.Errorf("registry.yaml lists no templates")
	}
	for _, entry := range reg.Templates {
		content, err := read(entry.Path)
		if err != nil {
			return nil, fmt.Errorf("registry entry %q: read %s: %w", entry.ID, entry.Path, err)
		}
		files[entry.Path] = content
	}
	// skills/ embedded packages: every file under skills/ travels with the
	// snapshot so ApplySquad can hand whole directories to SkillService.
	skillsRoot := filepath.Join(dir, "skills")
	if entries, err := os.ReadDir(skillsRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			_ = filepath.WalkDir(filepath.Join(skillsRoot, e.Name()), func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil // ignore (best effort — a broken skill dir fails at apply time)
				}
				rel, rerr := filepath.Rel(dir, p)
				if rerr != nil {
					return nil
				}
				if c, rerr := os.ReadFile(p); rerr == nil {
					files[rel] = string(c)
				}
				return nil
			})
		}
	}
	return files, nil
}

// parseTemplateFiles validates the fetched files into a cache snapshot.
// Every registry entry must parse and carry a consistent api_version/kind/id;
// a bad template is reported in Errors and skipped (the rest stay usable).
func parseTemplateFiles(files map[string]string) (*templateCache, error) {
	var reg templateRegistry
	if err := yaml.Unmarshal([]byte(files["registry.yaml"]), &reg); err != nil {
		return nil, fmt.Errorf("parse registry.yaml: %w", err)
	}
	cache := &templateCache{}
	for _, entry := range reg.Templates {
		content := files[entry.Path]
		var meta TemplateMeta
		if err := yaml.Unmarshal([]byte(content), &meta); err != nil {
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: parse metadata: %v", entry.Path, err))
			continue
		}
		// "not: a template" parses as an empty TemplateMeta — a template file
		// must at least carry api_version + kind to be considered one.
		if meta.APIVersion == "" && meta.Kind == "" && meta.ID == "" {
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: not a template file (missing api_version/kind)", entry.Path))
			continue
		}
		if !templateIDRe.MatchString(entry.ID) {
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: registry id %q is not kebab-case", entry.Path, entry.ID))
			continue
		}
		switch {
		case meta.APIVersion == "":
			meta.APIVersion = "agentwork/v1"
		case meta.APIVersion != "agentwork/v1":
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: unsupported api_version %q", entry.Path, meta.APIVersion))
			continue
		}
		// Registry is authoritative for identity; the file must not disagree.
		if meta.Kind == "" {
			meta.Kind = entry.Kind
		}
		if meta.Kind != entry.Kind {
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: kind %q disagrees with registry %q", entry.Path, meta.Kind, entry.Kind))
			continue
		}
		if meta.ID == "" {
			meta.ID = entry.ID
		}
		if meta.ID != entry.ID {
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: id %q disagrees with registry %q", entry.Path, meta.ID, entry.ID))
			continue
		}
		if meta.Name == "" {
			meta.Name = entry.Name
		}
		if meta.Description == "" {
			meta.Description = entry.Description
		}
		if meta.Version == "" {
			meta.Version = entry.Version
		}
		if len(meta.Tags) == 0 {
			meta.Tags = entry.Tags
		}
		if meta.Kind != TemplateKindSquad && meta.Kind != TemplateKindProject {
			cache.Errors = append(cache.Errors, fmt.Sprintf("template %s: kind must be squad-template or project-template, got %q", entry.Path, meta.Kind))
			continue
		}
		cache.Templates = append(cache.Templates, cachedTemplate{TemplateMeta: meta, Path: entry.Path, SpecYAML: content})
	}
	return cache, nil
}

// cached reads the snapshot from app_settings; not-found maps to a coded
// error (never fetched, or wiped).
func (s *TemplateService) cached(ctx context.Context) (*templateCache, error) {
	raw, err := s.settings.Get(ctx, templateCacheKey)
	if err != nil || raw == "" {
		return nil, NewCodedError(CodeTemplateFetchFailed, "模板尚未拉取——请先刷新模板列表")
	}
	var c templateCache
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("corrupt template cache: %w", err)
	}
	return &c, nil
}

// List returns the cached templates' metadata (optionally filtered by kind:
// "squad" | "project" | full kind | ” = all). An empty cache triggers a
// best-effort fetch so the first page load works with zero clicks.
func (s *TemplateService) List(ctx context.Context, kind string) ([]TemplateSummary, error) {
	c, err := s.cached(ctx)
	if err != nil {
		if _, rerr := s.Refresh(ctx, ""); rerr != nil {
			return nil, err // surface the original (coded) cache error
		}
		if c, err = s.cached(ctx); err != nil {
			return nil, err
		}
	}
	filter := normalizeKindFilter(kind)
	out := []TemplateSummary{}
	for _, t := range c.Templates {
		if filter != "" && t.Kind != filter {
			continue
		}
		out = append(out, TemplateSummary{TemplateMeta: t.TemplateMeta, Path: t.Path})
	}
	return out, nil
}

// Get returns one cached template in full (raw YAML + JSON spec).
func (s *TemplateService) Get(ctx context.Context, kind, id string) (*TemplateDetail, error) {
	c, err := s.cached(ctx)
	if err != nil {
		return nil, err
	}
	filter := normalizeKindFilter(kind)
	for _, t := range c.Templates {
		if t.ID != id || (filter != "" && t.Kind != filter) {
			continue
		}
		specJSON, err := yamlToJSON(t.SpecYAML)
		if err != nil {
			return nil, NewCodedErrorDetail(CodeTemplateInvalid,
				fmt.Sprintf("模板 %s 的 spec 无法解析：%v", id, err), nil)
		}
		return &TemplateDetail{TemplateMeta: t.TemplateMeta, Path: t.Path, SpecYAML: t.SpecYAML, Spec: specJSON}, nil
	}
	return nil, NewCodedErrorDetail(CodeTemplateNotFound,
		fmt.Sprintf("模板 %q 不存在（kind=%s）——刷新模板列表后重试", id, kind),
		map[string]any{"id": id, "kind": kind})
}

// LoadRaw returns one cached template's raw YAML (the apply path's input).
func (s *TemplateService) LoadRaw(ctx context.Context, kind, id string) (string, error) {
	c, err := s.cached(ctx)
	if err != nil {
		return "", err
	}
	filter := normalizeKindFilter(kind)
	for _, t := range c.Templates {
		if t.ID == id && (filter == "" || t.Kind == filter) {
			return t.SpecYAML, nil
		}
	}
	return "", NewCodedErrorDetail(CodeTemplateNotFound,
		fmt.Sprintf("模板 %q 不存在（kind=%s）", id, kind), map[string]any{"id": id, "kind": kind})
}

// SkillPackage returns the file map of an embedded skill package (skills/<name>/…)
// from the cached snapshot. ApplySquad uses it to hand whole packages to
// SkillService. ok=false when the package does not exist in the snapshot.
func (s *TemplateService) SkillPackage(ctx context.Context, name string) (map[string]string, bool, error) {
	c, err := s.cached(ctx)
	if err != nil {
		return nil, false, err
	}
	prefix := "skills/" + name + "/"
	files := map[string]string{}
	for path, content := range c.SkillFiles {
		if after, ok := strings.CutPrefix(path, prefix); ok && after != "" && after != "package.zip" {
			files[after] = content
		}
	}
	if len(files) == 0 {
		return nil, false, nil
	}
	return files, true, nil
}

// yamlToJSON converts a template YAML document to JSON (raw message) so the
// frontend can consume the decoded spec directly.
func yamlToJSON(doc string) (json.RawMessage, error) {
	var v any
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(false)
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if v == nil {
		return json.RawMessage("null"), nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// normalizeKindFilter maps the query filter ("squad" | "project" | full kind)
// to a full kind; ” = unfiltered.
func normalizeKindFilter(kind string) string {
	switch strings.TrimSpace(kind) {
	case "squad":
		return TemplateKindSquad
	case "project":
		return TemplateKindProject
	case TemplateKindSquad, TemplateKindProject:
		return strings.TrimSpace(kind)
	default:
		return ""
	}
}

// refreshTimeout bounds one clone — a dead host must not wedge the request.
const refreshTimeout = 2 * time.Minute
