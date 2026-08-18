package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestProbeCLIsDetectsFakeCLI plants a fake `claude` executable at the front
// of PATH and verifies the probe table picks it up (real machines resolve
// the same table against their actual PATH).
func TestProbeCLIsDetectsFakeCLI(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 2.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	clis := probeCLIs(context.Background(), nil)
	var found bool
	for _, c := range clis {
		if c.Name == "claude" {
			found = true
			if c.Version != "2.1.0" {
				t.Fatalf("expected version 2.1.0, got %q", c.Version)
			}
			if len(c.ACPSpawn) == 0 {
				t.Fatalf("expected acp spawn args")
			}
		}
	}
	if !found {
		t.Fatalf("fake claude not detected, got %+v", clis)
	}
}

// TestProbeCLIsDetectsCLIInScanDir plants a fake `claude` OUTSIDE PATH and
// verifies --scan resolves it — including the glob form (/opt/*/), and that
// the reported ACPSpawn carries the absolute executable.
func TestProbeCLIsDetectsCLIInScanDir(t *testing.T) {
	// A restricted PATH (only a shell) forces the PATH probe to miss every
	// CLI — the scan dirs must resolve them.
	bin := t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	base := t.TempDir()
	for _, sub := range []string{"alpha", "beta"} {
		dir := filepath.Join(base, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho 9.9.9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	clis := probeCLIs(context.Background(), []string{filepath.Join(base, "*")})
	var found bool
	for _, c := range clis {
		if c.Name == "claude" {
			found = true
			if c.ACPSpawn[0] != filepath.Join(base, "alpha", "claude") && c.ACPSpawn[0] != filepath.Join(base, "beta", "claude") {
				t.Fatalf("expected absolute spawn path from a scan dir, got %q", c.ACPSpawn[0])
			}
		}
	}
	if !found {
		t.Fatalf("fake claude in a --scan glob not detected, got %+v", clis)
	}
}

// TestScanDirsFor covers the --scan expansion rules: plain paths pass
// through, globs resolve to their DIRECTORY matches only.
func TestScanDirsFor(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	fileB := filepath.Join(base, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := scanDirsFor(filepath.Join(base, "*"))
	if len(got) != 1 || got[0] != dirA {
		t.Fatalf("glob must keep only directories, got %v", got)
	}
	if plain := scanDirsFor(dirA); len(plain) != 1 || plain[0] != dirA {
		t.Fatalf("plain path must pass through, got %v", plain)
	}
	// ** recurses: a nested directory two levels down still matches.
	deep := filepath.Join(base, "x", "y")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	deepGot := scanDirsFor(filepath.Join(base, "**"))
	found := false
	for _, d := range deepGot {
		if d == deep {
			found = true
		}
	}
	if !found {
		t.Fatalf("** must recurse into nested directories, got %v", deepGot)
	}
}

// TestConnectWSURL covers the server-flag → /connect WebSocket URL mapping.
func TestConnectWSURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:7373", "ws://127.0.0.1:7373/connect"},
		{"https://agentwork.example.com", "wss://agentwork.example.com/connect"},
		{"192.168.1.5:7373", "ws://192.168.1.5:7373/connect"},
	}
	for _, c := range cases {
		got := connectWSURL(c.in, "")
		if got != c.want {
			t.Fatalf("connectWSURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := connectWSURL("http://x", "tok"); got != "ws://x/connect?token=tok" {
		t.Fatalf("token must ride the query, got %q", got)
	}
}

// resetMergedProbeTable clears the package-level merged table after a test
// that set it, so later tests fall back to the builtin table.
func resetMergedProbeTable(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { mergedProbeTable = nil })
}

// TestMergeProbeTablesOverride: a custom entry with a builtin's Name
// overrides it (spawn/dirs change); the builtin's other fields are replaced
// wholesale by the custom entry.
func TestMergeProbeTablesOverride(t *testing.T) {
	custom := []probeEntry{{
		Name: "claude", ProbeCmd: "claude --version",
		ACPSpawn:         []string{"/opt/claude/claude", "--acp"},
		SkillsDir:        "~/.claude/skills",
		ProjectSkillsDir: ".claude/skills",
		ProfileFiles:     []string{"AGENTS.md"},
	}}
	merged := mergeProbeTables(builtinProbeTable, custom)
	var claude probeEntry
	for _, e := range merged {
		if e.Name == "claude" {
			claude = e
		}
	}
	if claude.ACPSpawn[0] != "/opt/claude/claude" {
		t.Fatalf("override must replace ACPSpawn, got %v", claude.ACPSpawn)
	}
	if !slices.Equal(claude.ProfileFiles, []string{"AGENTS.md"}) {
		t.Fatalf("override must replace ProfileFiles, got %v", claude.ProfileFiles)
	}
}

// TestMergeProbeTablesAdd: a custom entry with a new Name is appended.
func TestMergeProbeTablesAdd(t *testing.T) {
	custom := []probeEntry{{
		Name: "myagent", ProbeCmd: "myagent --version",
		ACPSpawn: []string{"myagent", "acp"},
	}}
	merged := mergeProbeTables(builtinProbeTable, custom)
	if !slices.ContainsFunc(merged, func(e probeEntry) bool { return e.Name == "myagent" }) {
		t.Fatalf("new custom entry must be appended, got %v", merged)
	}
}

// TestMergeProbeTablesBuiltinUnchanged: a builtin not mentioned in custom
// keeps its original values.
func TestMergeProbeTablesBuiltinUnchanged(t *testing.T) {
	custom := []probeEntry{{Name: "myagent", ProbeCmd: "x", ACPSpawn: []string{"x"}}}
	merged := mergeProbeTables(builtinProbeTable, custom)
	var openagent probeEntry
	for _, e := range merged {
		if e.Name == "openagent" {
			openagent = e
		}
	}
	if !slices.Equal(openagent.ACPSpawn, []string{"openagent", "serve", "--acp"}) {
		t.Fatalf("unmentioned builtin must keep its spawn, got %v", openagent.ACPSpawn)
	}
}

// TestMergeProbeTablesOrdering: builtins first (original order), then new
// custom entries (config-file order).
func TestMergeProbeTablesOrdering(t *testing.T) {
	custom := []probeEntry{
		{Name: "zagent", ProbeCmd: "z", ACPSpawn: []string{"z"}},
		{Name: "aagent", ProbeCmd: "a", ACPSpawn: []string{"a"}},
	}
	merged := mergeProbeTables(builtinProbeTable, custom)
	// First three are the builtins in original order.
	for i, want := range []string{"claude", "opencode", "openagent"} {
		if merged[i].Name != want {
			t.Fatalf("builtin %d must be %s, got %s (merged=%v)", i, want, merged[i].Name, merged)
		}
	}
	// New entries follow in config-file order (zagent before aagent).
	if len(merged) != 5 || merged[3].Name != "zagent" || merged[4].Name != "aagent" {
		t.Fatalf("new entries must follow in config order, got %v", merged)
	}
}

// TestLoadAgentsConfigValid: a valid YAML produces the right entries.
func TestLoadAgentsConfigValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - name: myagent
    probe_cmd: "myagent --version"
    acp_spawn: ["myagent", "acp"]
    skills_dir: "~/.myagent/skills"
    project_skills_dir: ".myagent/skills"
    profile_files: ["AGENTS.md"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := loadAgentsConfig(path)
	if err != nil {
		t.Fatalf("valid config must load, got %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "myagent" {
		t.Fatalf("expected one myagent entry, got %v", entries)
	}
	if entries[0].ProjectSkillsDir != ".myagent/skills" {
		t.Fatalf("project_skills_dir must parse, got %q", entries[0].ProjectSkillsDir)
	}
}

// TestLoadAgentsConfigMalformedEntry: entries missing required fields are
// skipped; valid entries in the same file are still returned.
func TestLoadAgentsConfigMalformedEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - name: no-probe
    acp_spawn: ["x"]
  - name: no-spawn
    probe_cmd: "x --version"
  - probe_cmd: "x"
    acp_spawn: ["x"]
  - name: good
    probe_cmd: "good --version"
    acp_spawn: ["good", "acp"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := loadAgentsConfig(path)
	if err != nil {
		t.Fatalf("load must not fail on malformed entries, got %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "good" {
		t.Fatalf("only the valid entry must remain, got %v", entries)
	}
}

// TestLoadAgentsConfigMissing: a missing file returns an error the caller
// can distinguish (os.IsNotExist).
func TestLoadAgentsConfigMissing(t *testing.T) {
	_, err := loadAgentsConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("missing file must return os.IsNotExist error, got %v", err)
	}
}

// TestActiveProbeTableFallback: nil mergedProbeTable → builtin; set → merged.
func TestActiveProbeTableFallback(t *testing.T) {
	resetMergedProbeTable(t)
	got := activeProbeTable()
	if len(got) != len(builtinProbeTable) {
		t.Fatalf("nil merged must fall back to builtin (len %d), got %v", len(builtinProbeTable), got)
	}
	for i, e := range builtinProbeTable {
		if got[i].Name != e.Name {
			t.Fatalf("nil merged must return builtin[%d]=%s, got %s", i, e.Name, got[i].Name)
		}
	}
	mergedProbeTable = []probeEntry{{Name: "only-custom", ProbeCmd: "x", ACPSpawn: []string{"x"}}}
	if got := activeProbeTable(); len(got) != 1 || got[0].Name != "only-custom" {
		t.Fatalf("set merged must return it, got %v", got)
	}
}

// TestProjectSkillsDirsWithCustom: after setMergedProbeTable adds a custom
// entry with a new ProjectSkillsDir, projectSkillsDirs() includes it (the
// git-exclude set must cover custom-CLI skills).
func TestProjectSkillsDirsWithCustom(t *testing.T) {
	resetMergedProbeTable(t)
	custom := []probeEntry{{
		Name: "myagent", ProbeCmd: "myagent --version",
		ACPSpawn:         []string{"myagent", "acp"},
		ProjectSkillsDir: ".myagent/skills",
	}}
	mergedProbeTable = mergeProbeTables(builtinProbeTable, custom)
	dirs := projectSkillsDirs()
	if !slices.Contains(dirs, ".myagent/skills") {
		t.Fatalf("custom ProjectSkillsDir must be in the exclude set, got %v", dirs)
	}
	// Builtins' dirs still present.
	if !slices.Contains(dirs, ".claude/skills") {
		t.Fatalf("builtin ProjectSkillsDir must still be present, got %v", dirs)
	}
}

// TestProbeCLIsWithCustomEntry: a custom entry whose ProbeCmd points at a
// fake script on PATH is probed and returns its custom ACPSpawn.
func TestProbeCLIsWithCustomEntry(t *testing.T) {
	resetMergedProbeTable(t)
	// A fake 'myagent' on PATH (prepend so sh stays resolvable for sh -c).
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "myagent"), []byte("#!/bin/sh\necho 1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	mergedProbeTable = []probeEntry{{
		Name: "myagent", ProbeCmd: "myagent --version",
		ACPSpawn:         []string{"myagent", "acp"},
		ProjectSkillsDir: ".myagent/skills",
	}}
	clis := probeCLIs(context.Background(), nil)
	var found bool
	for _, c := range clis {
		if c.Name == "myagent" {
			found = true
			if c.Version != "1.2.3" {
				t.Fatalf("version must be probed, got %q", c.Version)
			}
			if !slices.Equal(c.ACPSpawn, []string{"myagent", "acp"}) {
				t.Fatalf("custom ACPSpawn must be carried, got %v", c.ACPSpawn)
			}
		}
	}
	if !found {
		t.Fatalf("custom entry must be probed, got %+v", clis)
	}
}

// TestSetMergedProbeTableMissingFile: a missing config path leaves the
// merged table nil (activeProbeTable falls back to builtins), no error.
func TestSetMergedProbeTableMissingFile(t *testing.T) {
	resetMergedProbeTable(t)
	if err := setMergedProbeTable(filepath.Join(t.TempDir(), "absent.yaml")); err != nil {
		t.Fatalf("missing config must not error, got %v", err)
	}
	if mergedProbeTable != nil {
		t.Fatalf("missing config must leave merged table nil, got %v", mergedProbeTable)
	}
}

// TestSetMergedProbeTableHotReloadClearsOnDelete: the hot-reload contract.
// After a valid config sets mergedProbeTable, DELETING (or emptying) the file
// on a later reload must clear mergedProbeTable back to nil — so the builtin
// table is restored without restarting connect. A stale custom table must NOT
// survive the config's removal.
func TestSetMergedProbeTableHotReloadClearsOnDelete(t *testing.T) {
	resetMergedProbeTable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - name: myagent
    probe_cmd: "myagent --version"
    acp_spawn: ["myagent", "acp"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if mergedProbeTable == nil {
		t.Fatal("merged table must be set after a valid config")
	}
	// Delete the file → next reload must clear the cached table.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err != nil {
		t.Fatalf("reload after delete must not error, got %v", err)
	}
	if mergedProbeTable != nil {
		t.Fatalf("deleting config must clear merged table (restore builtins), got %v", mergedProbeTable)
	}
}

// TestSetMergedProbeTableHotReloadClearsOnEmpty: same contract, but the file
// is EMPTIED (zero valid entries) rather than deleted — also must clear.
func TestSetMergedProbeTableHotReloadClearsOnEmpty(t *testing.T) {
	resetMergedProbeTable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - name: myagent
    probe_cmd: "myagent --version"
    acp_spawn: ["myagent", "acp"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	// Rewrite to an empty config → reload must clear.
	if err := os.WriteFile(path, []byte("agents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err != nil {
		t.Fatalf("reload after empty must not error, got %v", err)
	}
	if mergedProbeTable != nil {
		t.Fatalf("emptying config must clear merged table, got %v", mergedProbeTable)
	}
}

// TestSetMergedProbeTableHotReloadKeepsPreviousOnParseError: a corrupted
// (unparseable) file on reload must NOT clear a previously loaded table — a
// half-edited config shouldn't drop a working one. The error is returned so
// the caller can warn, but mergedProbeTable stays as it was.
func TestSetMergedProbeTableHotReloadKeepsPreviousOnParseError(t *testing.T) {
	resetMergedProbeTable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - name: myagent
    probe_cmd: "myagent --version"
    acp_spawn: ["myagent", "acp"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	prev := mergedProbeTable
	if prev == nil {
		t.Fatal("merged table must be set after a valid config")
	}
	// Corrupt the file with invalid YAML → reload must error but NOT clear.
	if err := os.WriteFile(path, []byte("agents: [this is not: valid: yaml: ]:\n  - oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err == nil {
		t.Fatal("corrupted config must return an error")
	}
	// Same identity (not cleared, not rebuilt) — the previous table survives.
	if len(mergedProbeTable) != len(prev) {
		t.Fatalf("parse error must keep the previous table (len %d), got %v", len(prev), mergedProbeTable)
	}
	hasMyAgent := false
	for _, e := range mergedProbeTable {
		if e.Name == "myagent" {
			hasMyAgent = true
		}
	}
	if !hasMyAgent {
		t.Fatalf("previous custom entry must survive a parse error, got %v", mergedProbeTable)
	}
}

// TestSetMergedProbeTableLoadsAndMerges: a real config file is loaded and
// merged into mergedProbeTable.
func TestSetMergedProbeTableLoadsAndMerges(t *testing.T) {
	resetMergedProbeTable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yaml")
	if err := os.WriteFile(path, []byte(`agents:
  - name: myagent
    probe_cmd: "myagent --version"
    acp_spawn: ["myagent", "acp"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setMergedProbeTable(path); err != nil {
		t.Fatalf("load must succeed, got %v", err)
	}
	if mergedProbeTable == nil {
		t.Fatal("merged table must be set after a valid config")
	}
	if !slices.ContainsFunc(mergedProbeTable, func(e probeEntry) bool { return e.Name == "myagent" }) {
		t.Fatalf("custom entry must be in merged table, got %v", mergedProbeTable)
	}
	if !slices.ContainsFunc(mergedProbeTable, func(e probeEntry) bool { return e.Name == "claude" }) {
		t.Fatalf("builtins must still be present, got %v", mergedProbeTable)
	}
}
