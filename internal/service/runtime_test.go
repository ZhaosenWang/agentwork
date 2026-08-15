package service

import (
	"context"
	"errors"
	"testing"
)

// TestGetOrCreateDedupsByEndpoint: the acp-ws "no runtime entry point" path —
// selecting the same ws address repeatedly must reuse the first runtime, not
// insert a second row.
func TestGetOrCreateDedupsByEndpoint(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	svc := NewRuntimeService(st)

	first, err := svc.GetOrCreate(ctx, Runtime{Endpoint: "ws://127.0.0.1:8787/"})
	if err != nil {
		t.Fatalf("first get-or-create: %v", err)
	}
	if first.Transport != "ws" || first.Provider != "acp" {
		t.Fatalf("defaults: transport=%q provider=%q (want ws/acp)", first.Transport, first.Provider)
	}
	if first.Name != "127.0.0.1:8787" {
		t.Fatalf("derived name: got %q, want 127.0.0.1:8787", first.Name)
	}

	// Same endpoint, different (ignored) name → same row, no duplicate.
	second, err := svc.GetOrCreate(ctx, Runtime{Endpoint: "ws://127.0.0.1:8787/", Name: "should-be-ignored"})
	if err != nil {
		t.Fatalf("second get-or-create: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("same endpoint must reuse the existing runtime (got %q vs %q)", second.ID, first.ID)
	}

	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("runtime table must hold exactly 1 row, got %d", n)
	}
}

// TestGetOrCreateExplicitName uses the caller's name when it supplies one.
func TestGetOrCreateExplicitName(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	svc := NewRuntimeService(st)

	rt, err := svc.GetOrCreate(ctx, Runtime{Endpoint: "ws://example.com:9999", Name: "my-agent"})
	if err != nil {
		t.Fatalf("get-or-create: %v", err)
	}
	if rt.Name != "my-agent" {
		t.Fatalf("name = %q, want my-agent", rt.Name)
	}
}

// TestGetOrCreateRequiresEndpoint rejects an empty ws address.
func TestGetOrCreateRequiresEndpoint(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	svc := NewRuntimeService(st)

	_, err := svc.GetOrCreate(ctx, Runtime{})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}
