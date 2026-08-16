// Package zipx is the shared skill-package archive handling: the platform
// stores a skill as a zip (SKILL.md + scripts/references/binary assets)
// and pushes the ORIGINAL archive to the machine, which extracts it into
// the agent's staging dir. Both sides use this extractor — the same
// traversal/symlink defenses, enforced at both ends.
package zipx

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxZipSize caps an accepted skill archive.
const MaxZipSize = 10 << 20 // 10MB

// MaxTotalSize caps the extracted total (zip bombs).
const MaxTotalSize = 32 << 20 // 32MB

// Extract unpacks a skill zip under dest (created). Rejects absolute
// paths, ".." traversal, symlinks, and over-limit payloads — the archive
// comes from a human upload and must never escape the skill dir.
func Extract(data []byte, dest string) error {
	if len(data) == 0 {
		return fmt.Errorf("empty archive")
	}
	if len(data) > MaxZipSize {
		return fmt.Errorf("archive too large (%d bytes, max %d)", len(data), MaxZipSize)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not a valid zip: %w", err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	prefix := stripSingleTopDir(zr.File)
	var total int64
	for _, f := range zr.File {
		name := f.Name
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue // the stripped top-level directory itself
			}
		}
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, `\`) {
			return fmt.Errorf("unsafe path %q in archive", name)
		}
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %q in archive", name)
		}
		total += int64(f.UncompressedSize64)
		if total > MaxTotalSize {
			return fmt.Errorf("archive extracts over the limit (%d bytes)", MaxTotalSize)
		}
		path := filepath.Join(dest, filepath.FromSlash(name))
		if !strings.HasPrefix(path, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path %q in archive", name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if cerr != nil {
			return fmt.Errorf("extract %s: %w", name, cerr)
		}
	}
	return nil
}

// stripSingleTopDir detects the common "zip the whole folder" shape — every
// entry nested under one top-level directory (`zip -r skill.zip aliyunrds/`).
// It returns that directory's name with a trailing slash to strip, or "" when
// the archive is already flat (or has multiple top-level entries, which would
// make stripping ambiguous).
func stripSingleTopDir(files []*zip.File) string {
	top := ""
	for _, f := range files {
		name := f.Name
		if name == "" {
			continue
		}
		trimmed := strings.TrimSuffix(name, "/")
		if trimmed == "" {
			continue // the archive root itself
		}
		idx := strings.IndexByte(trimmed, '/')
		var comp string
		if idx < 0 {
			// A root-level entry: a file means the archive is already flat
			// (never strip); a directory is a candidate top-level dir.
			if !f.FileInfo().IsDir() {
				return ""
			}
			comp = trimmed
		} else {
			comp = trimmed[:idx]
		}
		// A traversal/absolute/empty top component must never be stripped —
		// those entries fall through to the unsafe-path rejection in Extract.
		if comp == "" || comp == "." || comp == ".." || strings.ContainsAny(comp, `/\`) {
			return ""
		}
		if top == "" {
			top = comp
		} else if top != comp {
			return "" // entries under different top-level paths
		}
	}
	if top == "" {
		return ""
	}
	return top + "/"
}

// Build assembles a skill zip from a path→content map (the platform's
// text-block upload mode produces the same archive shape as a real zip).
func Build(files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, content := range files {
		if strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, `\`) {
			return nil, fmt.Errorf("unsafe path %q", path)
		}
		w, err := zw.Create(path)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(w, content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
