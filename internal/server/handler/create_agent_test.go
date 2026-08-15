package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

func newAgentHandlers(t *testing.T) (*Handlers, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	h := &Handlers{
		Runtime: service.NewRuntimeService(st),
		Agent:   service.NewAgentService(st, bus),
	}
	return h, st
}

func postJSON(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestCreateAgentWithInlineRuntime: POST /agents with an inline ws runtime
// get-or-creates the runtime, binds the agent, and reuses it on repeat.
func TestCreateAgentWithInlineRuntime(t *testing.T) {
	h, st := newAgentHandlers(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := postJSON(t, mux, "/agents", `{"name":"a1","runtime":{"endpoint":"ws://127.0.0.1:8787/"},"max_concurrent":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first create: status %d body %s", rec.Code, rec.Body.String())
	}

	var id, name, transport, provider string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id, name, transport, provider FROM runtime WHERE endpoint='ws://127.0.0.1:8787/'`).
		Scan(&id, &name, &transport, &provider); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if transport != "ws" || provider != "acp" {
		t.Fatalf("runtime defaults: transport=%q provider=%q (want ws/acp)", transport, provider)
	}
	if name != "127.0.0.1:8787" {
		t.Fatalf("derived name = %q, want 127.0.0.1:8787", name)
	}

	// Repeat with the same endpoint → reuse the same runtime row.
	rec = postJSON(t, mux, "/agents", `{"name":"a2","runtime":{"endpoint":"ws://127.0.0.1:8787/"},"max_concurrent":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second create: status %d body %s", rec.Code, rec.Body.String())
	}
	var total int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("runtime table must hold 1 row after reuse, got %d", total)
	}

	// Agent is bound to the created runtime.
	var bound int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE runtime_id=?`, id).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 2 {
		t.Fatalf("expected 2 agents bound to runtime %s, got %d", id, bound)
	}
}

// TestCreateAgentBackwardCompat: an explicit runtime_id still works and wins
// over an inline runtime; a body with neither is rejected.
func TestCreateAgentBackwardCompat(t *testing.T) {
	h, st := newAgentHandlers(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	h.Mount(mux)

	rt, err := h.Runtime.Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// runtime_id + inline runtime both set → runtime_id wins, no inline insert.
	rec := postJSON(t, mux, "/agents",
		`{"name":"a1","runtime_id":"`+rt.ID+`","runtime":{"endpoint":"ws://ignored:1"},"max_concurrent":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create with runtime_id: status %d body %s", rec.Code, rec.Body.String())
	}
	var wsCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime WHERE endpoint='ws://ignored:1'`).Scan(&wsCount); err != nil {
		t.Fatal(err)
	}
	if wsCount != 0 {
		t.Fatalf("inline runtime must be ignored when runtime_id is set, found %d row(s)", wsCount)
	}

	// Neither runtime_id nor runtime → 400.
	rec = postJSON(t, mux, "/agents", `{"name":"a2","max_concurrent":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing runtime, got %d body %s", rec.Code, rec.Body.String())
	}
}
