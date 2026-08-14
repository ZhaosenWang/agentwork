package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned by Get* when no row matches.
var ErrNotFound = errors.New("not found")

// ErrValidation is returned for bad client input (missing/invalid fields).
// Handlers map it to HTTP 400.
var ErrValidation = errors.New("validation error")

// validationError wraps a message with ErrValidation so errors.Is works.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Unwrap() error { return ErrValidation }

// NewValidationError returns an error that satisfies errors.Is(err, ErrValidation).
func NewValidationError(msg string) error { return &validationError{msg: msg} }

// newID returns a 16-byte hex id (32 chars). Good enough for a single-user
// local store; not a UUID but unique in practice.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// now returns an RFC3339 timestamp.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ── filesystem layout helpers (shared with the daemon) ──

// RunsRoot is the root of all run/domain filesystem state (~/.agentwork/runs).
func RunsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentwork-runs")
	}
	return filepath.Join(home, ".agentwork", "runs")
}

// SanitizeDirName turns a domain name into a safe directory name — a
// hostile or odd name must not escape the scratch root (no /, no ..).
func SanitizeDirName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= 0x4e00 && r <= 0x9fff, // CJK unified
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_') // spaces, slashes, dots, everything else
		}
	}
	out := strings.Trim(b.String(), "_-")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "domain"
	}
	return out
}

// ScratchDomainRoot is a scratch domain's persistent project root (named by
// the sanitized domain name — a human can browse it).
func ScratchDomainRoot(domainName string) string {
	return filepath.Join(RunsRoot(), "scratch", SanitizeDirName(domainName))
}

// ScratchGoalDir is a scratch goal's persistent project directory — the
// branch analog: the goal's durable state lives HERE across runs.
func ScratchGoalDir(domainName, goalID string) string {
	return filepath.Join(ScratchDomainRoot(domainName), "goals", goalID)
}
