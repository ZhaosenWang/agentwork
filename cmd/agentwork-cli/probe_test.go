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

	clis := probeCLIs(context.Background())
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
