package zipx

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractRoundtrip: a Build'd archive extracts with every file intact.
func TestExtractRoundtrip(t *testing.T) {
	archive, err := Build(map[string]string{
		"SKILL.md":         "---\nname: demo\n---\nbody\n",
		"scripts/check.sh": "#!/bin/sh\necho ok\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Extract(archive, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "scripts", "check.sh"))
	if err != nil || string(b) != "#!/bin/sh\necho ok\n" {
		t.Fatalf("extracted content mismatch: %q %v", b, err)
	}
}

// TestExtractRejectsTraversal: ".." and absolute paths must never escape
// the destination (the archive comes from a human upload).
func TestExtractRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil.sh")
	_, _ = w.Write([]byte("bad"))
	_ = zw.Close()
	if err := Extract(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("a ../ path must be rejected")
	}

	buf.Reset()
	zw = zip.NewWriter(&buf)
	w, _ = zw.Create("/etc/evil")
	_, _ = w.Write([]byte("bad"))
	_ = zw.Close()
	if err := Extract(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("an absolute path must be rejected")
	}
}

// TestExtractStripsSingleTopDir: a "zip the whole folder" archive (every
// entry nested under one top-level dir) extracts with the wrapper removed,
// so SKILL.md lands at the destination root.
func TestExtractStripsSingleTopDir(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{
		"aliyunrds/",
		"aliyunrds/SKILL.md",
		"aliyunrds/scripts/rds-query.sh",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(name, "/") {
			_, _ = w.Write([]byte("content:" + name))
		}
	}
	_ = zw.Close()

	dest := t.TempDir()
	if err := Extract(buf.Bytes(), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md should be at the destination root after stripping: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "aliyunrds")); !os.IsNotExist(err) {
		t.Fatalf("top-level dir should be stripped, but aliyunrds/ still exists")
	}
	b, err := os.ReadFile(filepath.Join(dest, "scripts", "rds-query.sh"))
	if err != nil || string(b) != "content:aliyunrds/scripts/rds-query.sh" {
		t.Fatalf("nested file content mismatch: %q %v", b, err)
	}
}

// TestExtractKeepsMultipleTopDirs: an archive with more than one top-level
// entry must not be stripped (ambiguous) — files stay at their given paths.
func TestExtractKeepsMultipleTopDirs(t *testing.T) {
	archive, err := Build(map[string]string{
		"SKILL.md":     "root\n",
		"scripts/a.sh": "a\n",
		"docs/b.md":    "b\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Extract(archive, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Fatalf("root SKILL.md must be preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "docs", "b.md")); err != nil {
		t.Fatalf("sibling top-level dir must be preserved: %v", err)
	}
}
