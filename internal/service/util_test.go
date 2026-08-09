package service

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestErrNotFoundSentinel: ErrNotFound is a plain sentinel; errors.Is must
// match it exactly and nothing else. The store layer returns it bare and the
// handler maps it to 404 (handler.go).
func TestErrNotFoundSentinel(t *testing.T) {
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatal("ErrNotFound must satisfy errors.Is(ErrNotFound, ErrNotFound)")
	}
	if errors.Is(errors.New("not found"), ErrNotFound) {
		t.Fatal("a plain error with the same text is not the sentinel")
	}
	if errors.Is(ErrValidation, ErrNotFound) {
		t.Fatal("ErrValidation must not be ErrNotFound")
	}
}

// TestErrValidationSentinel: ErrValidation is a plain sentinel; errors.Is must
// match it exactly and nothing else. The handler maps it to 400 (handler.go).
func TestErrValidationSentinel(t *testing.T) {
	if !errors.Is(ErrValidation, ErrValidation) {
		t.Fatal("ErrValidation must satisfy errors.Is(ErrValidation, ErrValidation)")
	}
	if errors.Is(errors.New("validation error"), ErrValidation) {
		t.Fatal("a plain error with the same text is not the sentinel")
	}
	if errors.Is(ErrNotFound, ErrValidation) {
		t.Fatal("ErrNotFound must not be ErrValidation")
	}
}

// TestNewValidationError: every validation error must (a) carry its message,
// (b) satisfy errors.Is(err, ErrValidation) — including through %w wrapping as
// done in cron.go — and (c) NOT satisfy ErrNotFound.
func TestNewValidationError(t *testing.T) {
	cases := []string{
		"name is required",
		"assignee_type must be agent, squad, or human",
		"",
	}
	for _, msg := range cases {
		err := NewValidationError(msg)
		if err == nil {
			t.Fatalf("NewValidationError(%q) returned nil", msg)
		}
		if got := err.Error(); got != msg {
			t.Errorf("NewValidationError(%q).Error() = %q, want %q", msg, got, msg)
		}
		if !errors.Is(err, ErrValidation) {
			t.Errorf("NewValidationError(%q) must satisfy errors.Is(err, ErrValidation)", msg)
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("NewValidationError(%q) must not be ErrNotFound", msg)
		}
	}
}

// TestValidationErrorWrapped: code paths that wrap the sentinel with %w
// (cron.go: ParseExpression) must still be recognized by errors.Is.
func TestValidationErrorWrapped(t *testing.T) {
	inner := NewValidationError("bad cron")
	outer := errors.Join(inner, errors.New("context")) // join wraps without breaking Is
	if !errors.Is(outer, ErrValidation) {
		t.Fatal("errors.Join(NewValidationError, ...) must satisfy ErrValidation")
	}

	// fmt.Errorf with %w is the canonical wrap used in the codebase.
	wrapped := wrappedErr{ErrValidation}
	if !errors.Is(wrapped, ErrValidation) {
		t.Fatal("wrapped sentinel must satisfy errors.Is")
	}
}

// wrappedErr mimics fmt.Errorf("%w: ...") without extra imports.
type wrappedErr struct{ inner error }

func (w wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrappedErr) Unwrap() error { return w.inner }

// TestNewIDFormat: ids are 32 lowercase hex chars — the format every entity
// (agents, goals, runs, ...) relies on for its primary key.
func TestNewIDFormat(t *testing.T) {
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for i := 0; i < 100; i++ {
		if id := newID(); !hexRe.MatchString(id) {
			t.Fatalf("newID() = %q, want 32 lowercase hex chars", id)
		}
	}
}

// TestNewIDUnique: 1000 fresh ids must be pairwise distinct. crypto/rand makes
// collisions astronomically unlikely; any collision here is a real bug.
func TestNewIDUnique(t *testing.T) {
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
	ts := now()
	if !strings.HasSuffix(ts, "Z") {
		t.Errorf("now() = %q, want UTC (Z suffix)", ts)
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("now() = %q is not RFC3339Nano: %v", ts, err)
	}
	loc := parsed.Location()
	if loc != time.UTC {
		t.Errorf("now() parsed location = %v, want UTC", loc)
	}
	// Must not be in the future (clock sanity) and must be recent.
	if parsed.After(time.Now().Add(time.Minute)) {
		t.Errorf("now() = %q is more than 1m in the future", ts)
	}
}

// TestNowStable: two calls in quick succession may share the same timestamp
// (RFC3339Nano has ns resolution, but the clock can tick between calls) — the
// only hard requirement is that later calls never go backwards.
func TestNowMonotonic(t *testing.T) {
	first := time.Now().Add(-time.Second) // anchor before both calls
	a := now()
	b := now()
	ta, _ := time.Parse(time.RFC3339Nano, a)
	tb, _ := time.Parse(time.RFC3339Nano, b)
	if tb.Before(ta) {
		t.Errorf("now() went backwards: %q then %q", a, b)
	}
	if ta.Before(first) {
		t.Errorf("now() = %q is before the anchor %q", a, first.Format(time.RFC3339Nano))
	}
}
