package main

import (
	"context"
	"os"
	"path/filepath"
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
