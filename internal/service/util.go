package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
// code is a machine-readable error code (may be empty for plain validation
// errors); see codes.go for the shared set. detail is an optional set of
// structured parameters the frontend interpolates into the localized
// message template from the remote term.json (e.g. {name, count}).
type validationError struct {
	msg    string
	code   string
	detail map[string]any
}

func (e *validationError) Error() string                  { return e.msg }
func (e *validationError) Unwrap() error                  { return ErrValidation }
func (e *validationError) Code() string                   { return e.code }
func (e *validationError) Detail() map[string]any         { return e.detail }

// NewValidationError returns an error that satisfies errors.Is(err, ErrValidation).
func NewValidationError(msg string) error { return &validationError{msg: msg} }

// NewCodedError returns a validation error carrying a machine-readable code
// (see codes.go). Handlers surface the code in the JSON body so the frontend
// can branch on it instead of string-matching the message.
func NewCodedError(code, msg string) error {
	return &validationError{msg: msg, code: code}
}

// NewCodedErrorDetail is like NewCodedError but also carries structured
// parameters (detail) the frontend interpolates into the localized message
// template. For example, a dependency-conflict error passes {"name","count"}
// so the frontend can render "project {name} has {count} goals...".
func NewCodedErrorDetail(code, msg string, detail map[string]any) error {
	return &validationError{msg: msg, code: code, detail: detail}
}

// NewFieldRequiredError returns a CodeFieldRequired validation error for a
// missing required field. The field is the English identifier the frontend
// maps to a localized label via its fieldLabels table. Used for all simple
// "xxx is required" checks; validations with richer context stay on
// NewValidationError (→ CodeValidation fallback).
func NewFieldRequiredError(field string) error {
	return NewCodedErrorDetail(CodeFieldRequired, field+" is required", map[string]any{"field": field})
}

// CodedError is the interface a handler uses to extract the code. Both
// validationError and the not-found path satisfy it (writeErr falls back to
// CodeNotFound for ErrNotFound when the error has no Code() of its own).
type CodedError interface {
	Code() string
}

// DetailedError is the interface a handler uses to extract the structured
// detail parameters for frontend template interpolation.
type DetailedError interface {
	Detail() map[string]any
}

// dupNameCodedError returns a 400 coded error when err is a SQLite
// UNIQUE-name conflict (extended code SQLITE_CONSTRAINT_UNIQUE = 2067),
// or nil otherwise. kind is the entity label ("agent"/"domain"/...) for
// the message, code is the machine-readable code. Identified via errors.As
// on the driver's typed error — precise (excludes NOT NULL / FK / other
// constraints) and driver-typed. Callers return it directly on hit, else
// wrap the original err for a 500. The returned error carries detail{name}
// so the frontend can interpolate the localized "name {name} already exists" template.
func dupNameCodedError(err error, code, kind, name string) error {
	var se interface{ Code() int }
	if errors.As(err, &se) && se.Code() == 2067 {
		return NewCodedErrorDetail(code, fmt.Sprintf("%s %q already exists", kind, name), map[string]any{"name": name})
	}
	return nil
}

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
