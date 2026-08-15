package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/eushing/agentwork/internal/store"
)

// Skill is a platform-managed skill package (SKILL.md + resources) — the
// skills library agents get their skills from (CLI 分支 Phase 4). Files
// live on disk under <RunsRoot>/skills/<id>/; the machine receives them
// via config.push and installs them under agentwork-<name>/.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
}

// SkillService owns the skills library.
type SkillService struct {
	st *store.Store
}

// NewSkillService wires the library.
func NewSkillService(st *store.Store) *SkillService { return &SkillService{st: st} }

// skillDir returns the on-disk root of one skill's files.
func skillDir(skillID string) string {
	return filepath.Join(RunsRoot(), "skills", skillID)
}

// Create stores a skill package: the file map is written to disk
// (path → content; SKILL.md is required — a skill without instructions is
// a file dump, not a skill).
func (s *SkillService) Create(ctx context.Context, name, description string, files map[string]string) (*Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, NewValidationError("name is required")
	}
	if _, ok := files["SKILL.md"]; !ok || strings.TrimSpace(files["SKILL.md"]) == "" {
		return nil, NewValidationError("SKILL.md is required (the skill's instructions)")
	}
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM skill WHERE name=?`, name).Scan(&n); err != nil {
		return nil, fmt.Errorf("check skill name: %w", err)
	}
	if n > 0 {
		return nil, NewValidationError(fmt.Sprintf("skill %q already exists", name))
	}
	sk := &Skill{ID: newID(), Name: name, Description: description, CreatedAt: now()}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO skill (id,name,description,created_at) VALUES (?,?,?,?)`,
		sk.ID, sk.Name, sk.Description, sk.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert skill: %w", err)
	}
	if err := s.writeFiles(sk.ID, files); err != nil {
		_, _ = s.st.DB().ExecContext(ctx, `DELETE FROM skill WHERE id=?`, sk.ID)
		return nil, err
	}
	return sk, nil
}

// writeFiles materializes the skill's files under its directory. Paths are
// sanitized: no absolute paths, no .. — a skill must not write outside its
// own directory.
func (s *SkillService) writeFiles(skillID string, files map[string]string) error {
	dir := skillDir(skillID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir skill dir: %w", err)
	}
	for path, content := range files {
		clean := filepath.Clean(path)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid skill file path %q", path)
		}
		target := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// List returns the library (oldest first).
func (s *SkillService) List(ctx context.Context) ([]Skill, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id,name,description,created_at FROM skill ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Skill{}
	for rows.Next() {
		var sk Skill
		if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// Delete removes a skill from the library and its files.
func (s *SkillService) Delete(ctx context.Context, id string) error {
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE skills LIKE ?`, "%\""+id+"\"%").Scan(&n); err != nil {
		return fmt.Errorf("check skill usage: %w", err)
	}
	if n > 0 {
		return NewValidationError(fmt.Sprintf("skill is selected by %d agent(s) — unselect it first", n))
	}
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM skill WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	_ = os.RemoveAll(skillDir(id))
	return nil
}

// Files returns the skill's file map (path → content) for config.push.
func (s *SkillService) Files(ctx context.Context, id string) (map[string]string, error) {
	dir := skillDir(id)
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// SortedFilePaths returns the skill's file paths in a stable order.
func SortedFilePaths(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
