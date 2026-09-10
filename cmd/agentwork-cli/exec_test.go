package main

import (
	"strings"
	"testing"
)

// TestSanitizeNameMirrorsDaemon pins the CLI's scratch-name sanitizer to
// the daemon's sanitizeDirName behavior (internal/daemon/runaway_test.go).
// The two MUST stay in sync: the daemon embeds its computed path into the
// run's AGENTS.md, the agent obeys that absolute path over the spawn cwd,
// and the CLI reads the artifacts back from ITS computed workdir — a drift
// between the two sanitizers silently moves the artifacts out of reach
// (the CJK digest domain "AI知识精选" diverged exactly this way).
func TestSanitizeNameMirrorsDaemon(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// CJK must survive (the daemon keeps CJK unified ideographs) —
		// a Chinese domain name must sanitize identically on both sides.
		{"AI知识精选", "AI知识精选"},
		{"AI 动态·调研", "AI_动态_调研"},
		// Path-hostile input stays flat.
		{"../etc/passwd", "etc_passwd"},
		{"///", "domain"},
		{"", "domain"},
		{"plain", "plain"},
		{"has space-dash_", "has_space-dash"}, // trailing _ trimmed, like the daemon's
	}
	for _, c := range cases {
		got := sanitizeName(c.in)
		if got != c.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "/\\") || strings.Contains(got, "..") {
			t.Errorf("sanitizeName(%q) = %q: must not contain separators or ..", c.in, got)
		}
	}
}
