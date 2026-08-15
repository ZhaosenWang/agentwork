package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
// Code map for now — it graduates to a config table when the set grows.
var probeTable = []struct {
	Name         string
	ProbeCmd     string   // run with sh -c; success = installed
	ACPSpawn     []string // how to start it as an ACP stdio server
	SkillsDir    string   // ~ expanded
	ProfileFiles []string
}{
	{
		Name:      "claude",
		ProbeCmd:  "claude --version",
		ACPSpawn:  []string{"claude", "--acp"},
		SkillsDir: "~/.claude/skills",
		ProfileFiles: []string{"CLAUDE.md", "AGENTS.md"},
	},
	{
		Name:      "opencode",
		ProbeCmd:  "opencode --version",
		ACPSpawn:  []string{"opencode", "acp", "--pure"},
		SkillsDir: "~/.config/opencode/skills",
		ProfileFiles: []string{"AGENTS.md"},
	},
	{
		Name:      "openagent",
		ProbeCmd:  "openagent --version",
		ACPSpawn:  []string{"openagent", "acp"},
		SkillsDir: "~/.openagent/skills",
		ProfileFiles: []string{"SOUL.md", "SYSTEM.md", "AGENTS.md"},
	},
}

// probeCLIs scans the environment for the probe-table CLIs. Probe commands
// get a hard timeout derived from ctx (a hung version check must not stall
// registration; Ctrl+C aborts probing too).
func probeCLIs(ctx context.Context) []link.ProbeCLI {
	out := []link.ProbeCLI{}
	for _, e := range probeTable {
		if !probeRuns(ctx, e.ProbeCmd) {
			continue
		}
		out = append(out, link.ProbeCLI{
			Name:         e.Name,
			Version:      probeVersion(ctx, e.ProbeCmd),
			ACPSpawn:     e.ACPSpawn,
			SkillsDir:    expandHome(e.SkillsDir),
			ProfileFiles: e.ProfileFiles,
		})
	}
	return out
}

// probeRuns reports whether the probe command executes successfully.
func probeRuns(ctx context.Context, cmd string) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return exec.CommandContext(pctx, "sh", "-c", cmd).Run() == nil
}

// probeVersion runs the probe command and returns its first output line,
// trimmed ('' when the CLI prints nothing).
func probeVersion(ctx context.Context, cmd string) string {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	b, err := exec.CommandContext(pctx, "sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line
}

// expandHome resolves a leading ~ ('' input passes through).
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
