package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
