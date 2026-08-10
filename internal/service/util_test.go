// Tests for the shared helpers in util.go: the two error sentinels
// (ErrNotFound, ErrValidation), NewValidationError, newID, and now().
// Every other test in this package exercises these helpers indirectly,
// so this file pins down their contracts in isolation.
package service

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// parseNow parses an RFC3339Nano timestamp and fails the test if malformed.
func parseNow(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("invalid RFC3339Nano timestamp %q: %v", s, err)
	}
	return parsed
}

// TestSentinelsDistinct: the handler (handler.go) maps ErrNotFound→404 and
// ErrValidation→400 with an if/else chain, so the sentinels must be mutually
// exclusive. errors.Is is identity-based, not text-based: a plain error that
// merely shares the message must NOT match.
func TestSentinelsDistinct(t *testing.T) {
	t.Parallel()
	// Cross-match both ways: the handler's if/else chain depends on
	// ErrValidation never satisfying ErrNotFound and vice versa.
	if errors.Is(ErrValidation, ErrNotFound) {
		t.Error("ErrValidation must not satisfy ErrNotFound")
	}
	if errors.Is(ErrNotFound, ErrValidation) {
		t.Error("ErrNotFound must not satisfy ErrValidation")
	}
	// Same text ≠ same sentinel: the store layer returns ErrNotFound bare, and
	// any error with a matching message must not be mistaken for it.
	for _, tc := range []struct {
		sent error
		text string
	}{
		{ErrNotFound, "not found"},
		{ErrValidation, "validation error"},
	} {
		if errors.Is(errors.New(tc.text), tc.sent) {
			t.Errorf("plain error %q must not satisfy %v", tc.text, tc.sent)
		}
	}
}

// TestNewValidationError: a validation error must carry its message verbatim,
// satisfy errors.Is(err, ErrValidation) — both directly and through the
// %w-wrapped form cron.go uses — and never satisfy ErrNotFound.
func TestNewValidationError(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		"name is required",
		"assignee_type must be agent, squad, or human",
		"", // empty message must still produce a usable error
	} {
		// One subtest per message so a failure pinpoints the exact case.
		t.Run(fmt.Sprintf("message %q", msg), func(t *testing.T) {
			err := NewValidationError(msg)
			if err == nil {
				t.Fatalf("NewValidationError(%q) returned nil", msg)
			}
			if got := err.Error(); got != msg {
				t.Errorf("NewValidationError(%q).Error() = %q, want %q", msg, got, msg)
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("NewValidationError(%q) must satisfy ErrValidation", msg)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("NewValidationError(%q) must not satisfy ErrNotFound", msg)
			}
			// cron.go wraps the sentinel with %w; the wrapped error must stay
			// recognizable to the handler's errors.Is checks.
			wrapped := fmt.Errorf("%w: %s", err, "context")
			if !errors.Is(wrapped, ErrValidation) {
				t.Errorf("%%w-wrapped NewValidationError(%q) must satisfy ErrValidation", msg)
			}
		})
	}
}

// TestNewIDFormat: ids are 32 lowercase hex chars — the primary-key format
// every entity (agents, goals, runs, ...) relies on.
func TestNewIDFormat(t *testing.T) {
	t.Parallel()
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	// 100 draws: a structural bug (wrong length, bad alphabet, uppercase)
	// would fail on the very first draw, so the loop guards against a skewed
	// or occasionally-broken source rather than a broken format.
	for i := 0; i < 100; i++ {
		if id := newID(); !hexRe.MatchString(id) {
			t.Fatalf("newID() = %q, want 32 lowercase hex chars", id)
		}
	}
}

// TestNewIDUnique: fresh ids must be pairwise distinct. crypto/rand makes
// collisions astronomically unlikely, so any collision here is a real bug.
func TestNewIDUnique(t *testing.T) {
	t.Parallel()
	// 1000 draws into a map: a duplicate means the entropy source stopped
	// being random (e.g. a reused/static reader), not bad luck.
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := newID()
		if _, dup := seen[id]; dup {
			t.Fatalf("newID() collision: %q generated twice in %d calls", id, n)
		}
		seen[id] = struct{}{}
	}
}

// TestNowFormat: now() returns a UTC RFC3339Nano timestamp that round-trips
// through time.Parse — store columns and the API surface both depend on it.
func TestNowFormat(t *testing.T) {
	t.Parallel()
	ts := now()
	if !strings.HasSuffix(ts, "Z") {
		t.Errorf("now() = %q, want UTC (Z suffix)", ts)
	}
	// parseNow doubles as the round-trip check: it fails the test outright
	// if the string is not exactly RFC3339Nano.
	parsed := parseNow(t, ts)
	if parsed.Location() != time.UTC {
		t.Errorf("now() parsed location = %v, want UTC", parsed.Location())
	}
	// Clock sanity: never more than a minute in the future.
	if parsed.After(time.Now().Add(time.Minute)) {
		t.Errorf("now() = %q is more than 1m in the future", ts)
	}
}

// TestNowMonotonic: two calls in quick succession may share the same timestamp
// (RFC3339Nano has ns resolution, but the clock can tick between calls) — the
// only hard requirement is that later calls never go backwards.
func TestNowMonotonic(t *testing.T) {
	t.Parallel()
	a, b := now(), now()
	ta, tb := parseNow(t, a), parseNow(t, b)
	// A backwards clock would corrupt ordering assumptions in the store
	// layer (runs/goals sorted by timestamp), so monotonicity is the one
	// property that must always hold.
	if tb.Before(ta) {
		t.Errorf("now() went backwards: %q then %q", a, b)
	}
}
