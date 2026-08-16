package zipx

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
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
