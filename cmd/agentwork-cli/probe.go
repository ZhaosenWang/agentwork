package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/eushing/agentwork/internal/link"
)

// probeTimeout bounds a single probe command (a hung version check must
// not stall registration). Derived from the caller's ctx so Ctrl+C also
// aborts probing.
const probeTimeout = 5 * time.Second

// probeTable is the registry of agent CLIs the CLI knows how to detect
// (CLI 分支 Phase 1). Each entry maps a CLI to: the probe command (runs
// fast and prints a version), how to start it as an ACP stdio server, and
// where its skills / profile files live (config push lands in Phase 4).
// ProjectSkillsDir is the relative directory inside a run workdir where
// the CLI loads PROJECT-level skills — the executor stages the agent's
// skills there and the commit excludes every probed dir.
// Code map for now — it graduates to a config table when the set grows.
var probeTable = []struct {
	Name             string
	ProbeCmd         string   // run with sh -c; success = installed
	ACPSpawn         []string // how to start it as an ACP stdio server
	SkillsDir        string   // ~ expanded (global skills)
	ProjectSkillsDir string   // relative, inside a run workdir
	ProfileFiles     []string
}{
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
		Name:             "openagent",
		ProbeCmd:         "openagent --version",
		ACPSpawn:         []string{"openagent", "serve", "--acp"},
		SkillsDir:        "~/.openagent/skills",
		ProjectSkillsDir: ".agents/skills", // the AgentSkills standard (npx skills add)
		ProfileFiles:     []string{"SOUL.md", "SYSTEM.md", "AGENTS.md"},
	},
}

// projectSkillsDirs returns every project-level skills directory the probe
// table knows — the git exclusion set (staged skills are platform
// infrastructure, never the agent's commit) and the executor's fallback.
func projectSkillsDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range probeTable {
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
	for _, e := range probeTable {
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
