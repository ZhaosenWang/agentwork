package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/eushing/agentwork/internal/link"
	"gopkg.in/yaml.v3"
)

// probeTimeout bounds a single probe command (a hung version check must
// not stall registration). Derived from the caller's ctx so Ctrl+C also
// aborts probing.
const probeTimeout = 5 * time.Second

// probeEntry is one agent CLI probe-table entry (builtin or user-configured
// via ~/.agentwork/agents.yaml). Each entry maps a CLI to: the probe command
// (runs fast and prints a version), how to start it as an ACP stdio server,
// and where its skills / profile files live (config push lands the skills).
// ProjectSkillsDir is the relative directory inside a run workdir where the
// CLI loads PROJECT-level skills — the executor stages the agent's skills
// there and the commit excludes every probed dir.
type probeEntry struct {
	Name             string   `yaml:"name"`
	ProbeCmd         string   `yaml:"probe_cmd"`          // run with sh -c; success = installed
	ACPSpawn         []string `yaml:"acp_spawn"`          // how to start it as an ACP stdio server
	SkillsDir        string   `yaml:"skills_dir"`         // ~ expanded (global skills)
	ProjectSkillsDir string   `yaml:"project_skills_dir"` // relative, inside a run workdir
	ProfileFiles     []string `yaml:"profile_files"`
}

// agentsConfig is the top-level shape of the ~/.agentwork/agents.yaml file.
type agentsConfig struct {
	Agents []probeEntry `yaml:"agents"`
}

// builtinProbeTable is the hardcoded registry of agent CLIs the CLI knows
// how to detect (CLI 分支 Phase 1). A user's agents.yaml can ADD entries
// (new CLIs) or OVERRIDE these by Name — see mergeProbeTables.
var builtinProbeTable = []probeEntry{
	{
		Name:             "claude",
		ProbeCmd:         "claude --version",
		ACPSpawn:         []string{"claude", "--acp"},
		SkillsDir:        "~/.claude/skills",
		ProjectSkillsDir: ".claude/skills",
		ProfileFiles:     []string{"CLAUDE.md", "AGENTS.md"},
	},
	{
		Name:             "opencode",
		ProbeCmd:         "opencode --version",
		ACPSpawn:         []string{"opencode", "acp", "--pure"},
		SkillsDir:        "~/.config/opencode/skills",
		ProjectSkillsDir: ".opencode/skill",
		ProfileFiles:     []string{"AGENTS.md"},
	},
	{
		Name:             "hwcloud",
		ProbeCmd:         "hwcloud --version",
		ACPSpawn:         []string{"hwcloud", "serve", "--acp"},
		SkillsDir:        "~/.agents/skills",
		ProjectSkillsDir: ".agents/skills", // the AgentSkills standard (npx skills add)
		ProfileFiles:     []string{"SOUL.md", "SYSTEM.md", "AGENTS.md"},
	},
	{
		Name:             "kimi",
		ProbeCmd:         "kimi --version",
		ACPSpawn:         []string{"kimi", "acp"},
		SkillsDir:        "~/.kimi/skills",
		ProjectSkillsDir: ".kimi/skills",
		ProfileFiles:     []string{"AGENTS.md"},
	},
	{
		Name:             "hermes",
		ProbeCmd:         "hermes-acp --version",
		ACPSpawn:         []string{"hermes-acp"},
		SkillsDir:        "~/.hermes/skills",
		ProjectSkillsDir: "",
		ProfileFiles:     []string{"AGENTS.md"},
	},
	{
		Name:             "openclaw",
		ProbeCmd:         "/root/runtime/openclaw/openclaw.mjs --version",
		ACPSpawn:         []string{"/root/runtime/openclaw/node/bin/node", "/root/runtime/openclaw/openclaw.mjs", "acp"},
		SkillsDir:        "~/.openclaw/skills",
		ProjectSkillsDir: ".agents/skills",
		ProfileFiles:     []string{"AGENTS.md"},
	},
	{
		Name:             "jiuwenswarm",
		ProbeCmd:         "jiuwenswarm-acp --help",
		ACPSpawn:         []string{"jiuwenswarm-acp"},
		SkillsDir:        "~/.jiuwenswarm/service_default/agent_default/jiuwenswarm_workspace/skills",
		ProjectSkillsDir: "",
		ProfileFiles:     []string{"AGENTS.md"},
	},
	{
		Name:             "codearts",
		ProbeCmd:         "codearts --version",
		ACPSpawn:         []string{"codearts", "acp"},
		SkillsDir:        "~/.codeartsdoer/skills",
		ProjectSkillsDir: ".codeartsdoer/skills",
		ProfileFiles:     []string{"AGENTS.md"},
	},
}

// mergedProbeTable holds the builtin table + agents.yaml overrides, loaded
// once at connect startup by setMergedProbeTable. nil = no config loaded →
// activeProbeTable falls back to the builtin table (so tests and a missing
// config file still probe the builtins).
var mergedProbeTable []probeEntry

// activeProbeTable returns the merged probe table when a config has been
// loaded, else the builtin table. Both probeCLIs and projectSkillsDirs
// consume this so a custom entry's ProjectSkillsDir lands in the git-exclude
// set and its CLI gets probed.
func activeProbeTable() []probeEntry {
	if mergedProbeTable != nil {
		return mergedProbeTable
	}
	return builtinProbeTable
}

// mergeProbeTables merges custom (agents.yaml) entries into the builtin
// table. Entries sharing a builtin's Name OVERRIDE it (customize a known
// CLI's spawn command / dirs); new Names are appended in config-file order.
// Builtins not mentioned in custom are kept unchanged. Result order:
// builtins first (original order, with overrides applied), then new custom
// entries (config-file order).
func mergeProbeTables(builtin, custom []probeEntry) []probeEntry {
	byName := map[string]probeEntry{}
	for _, e := range builtin {
		byName[e.Name] = e
	}
	for _, e := range custom {
		byName[e.Name] = e // override-by-name: custom wins
	}
	merged := make([]probeEntry, 0, len(builtin)+len(custom))
	// Builtins first, with any override applied.
	for _, e := range builtin {
		merged = append(merged, byName[e.Name])
		delete(byName, e.Name)
	}
	// Remaining are new custom entries — preserve config-file order.
	for _, e := range custom {
		if _, ok := byName[e.Name]; ok {
			merged = append(merged, e)
			delete(byName, e.Name)
		}
	}
	return merged
}

// loadAgentsConfig reads and validates the agents YAML config. Each entry
// needs at least name + probe_cmd + acp_spawn; an entry missing any of
// those is warned and skipped (never fatal — the remaining valid entries
// are still returned). Returns (nil, nil) for an empty/no-valid-entry file.
// A missing file returns the os error so the caller can distinguish "no
// config" from "bad config".
func loadAgentsConfig(path string) ([]probeEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg agentsConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	valid := make([]probeEntry, 0, len(cfg.Agents))
	for i, e := range cfg.Agents {
		if e.Name == "" {
			cliLogf("agents config: entry %d skipped: name is required", i+1)
			continue
		}
		if e.ProbeCmd == "" {
			cliLogf("agents config: entry %q skipped: probe_cmd is required", e.Name)
			continue
		}
		if len(e.ACPSpawn) == 0 {
			cliLogf("agents config: entry %q skipped: acp_spawn is required", e.Name)
			continue
		}
		valid = append(valid, e)
	}
	return valid, nil
}

// setMergedProbeTable loads the agents config from path and merges it with
// the builtin table, caching the result in mergedProbeTable. A missing file
// or a config with no valid entries clears mergedProbeTable (activeProbeTable
// then falls back to builtins) — so deleting or emptying the config restores
// the builtin table on the next reload, no restart needed. A YAML parse
// error returns the error WITHOUT touching mergedProbeTable: the caller keeps
// the previously loaded table (a transiently-corrupted edit shouldn't drop a
// working custom table). Called at connect startup and on every probe tick
// (hot-reload: edits to ~/.agentwork/agents.yaml take effect within a minute).
func setMergedProbeTable(path string) error {
	custom, err := loadAgentsConfig(path)
	if os.IsNotExist(err) {
		mergedProbeTable = nil // file removed → restore builtin fallback
		return nil
	}
	if err != nil {
		return err // parse error: caller warns, KEEPS the previous table
	}
	if len(custom) == 0 {
		mergedProbeTable = nil // empty / no valid entries → builtin fallback
		return nil
	}
	mergedProbeTable = mergeProbeTables(builtinProbeTable, custom)
	return nil
}

// projectSkillsDirs returns every project-level skills directory the probe
// table knows — the git exclusion set (staged skills are platform
// infrastructure, never the agent's commit) and the executor's fallback.
func projectSkillsDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range activeProbeTable() {
		if e.ProjectSkillsDir != "" && !seen[e.ProjectSkillsDir] {
			seen[e.ProjectSkillsDir] = true
			out = append(out, e.ProjectSkillsDir)
		}
	}
	return out
}

// probeCLIs scans the environment for the probe-table CLIs: PATH first,
// then each --scan directory (a CLI installed outside PATH — the common
// case for absolute-path installs). When a scan dir resolves the CLI, the
// reported ACPSpawn carries the ABSOLUTE executable so the runtime row
// spawns correctly on a machine whose PATH never sees it. Probe commands
// get a hard timeout derived from ctx (a hung version check must not stall
// registration; Ctrl+C aborts probing too).
func probeCLIs(ctx context.Context, scanDirs []string) []link.ProbeCLI {
	out := []link.ProbeCLI{}
	for _, e := range activeProbeTable() {
		spawn := e.ACPSpawn
		extraPath := "" // scan dir that resolved the CLI (empty = found on PATH)
		if !probeRuns(ctx, e.ProbeCmd, "") {
			for _, pattern := range scanDirs {
				for _, d := range scanDirsFor(pattern) {
					if abs, err := filepath.Abs(d); err == nil {
						d = abs
					}
					if !probeRuns(ctx, e.ProbeCmd, d) {
						continue
					}
					extraPath = d
					break
				}
				if extraPath != "" {
					break
				}
			}
			if extraPath == "" {
				continue
			}
		}
		if extraPath != "" {
			spawn = append([]string{filepath.Join(extraPath, e.Name)}, e.ACPSpawn[1:]...)
		}
		out = append(out, link.ProbeCLI{
			Name:             e.Name,
			Version:          probeVersion(ctx, e.ProbeCmd, extraPath),
			ACPSpawn:         spawn,
			SkillsDir:        expandHome(e.SkillsDir),
			ProjectSkillsDir: e.ProjectSkillsDir,
			ProfileFiles:     e.ProfileFiles,
		})
	}
	return out
}

// scanDirsFor expands one --scan argument into concrete directories: ~ is
// expanded, glob patterns are resolved via doublestar (`*` one segment,
// `**` recursive — /opt/**/ = any depth under /opt), and non-directory
// matches are dropped. Plain paths pass through unchanged.
func scanDirsFor(pattern string) []string {
	pattern = expandHome(pattern)
	if !strings.ContainsAny(pattern, "*?[") {
		return []string{pattern}
	}
	matches, err := doublestar.FilepathGlob(pattern)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.IsDir() {
			out = append(out, m)
		}
	}
	return out
}

// probeRuns reports whether the probe command executes successfully, with
// extraPath (a --scan dir) prepended to PATH when non-empty.
func probeRuns(ctx context.Context, cmd, extraPath string) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c := exec.CommandContext(pctx, "sh", "-c", cmd)
	if extraPath != "" {
		c.Env = append(os.Environ(), "PATH="+extraPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return c.Run() == nil
}

// probeVersion runs the probe command and returns its first output line,
// trimmed (” when the CLI prints nothing).
func probeVersion(ctx context.Context, cmd, extraPath string) string {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c := exec.CommandContext(pctx, "sh", "-c", cmd)
	if extraPath != "" {
		c.Env = append(os.Environ(), "PATH="+extraPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	b, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line
}

// expandHome resolves a leading ~ (” input passes through).
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}
