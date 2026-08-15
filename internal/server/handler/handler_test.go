package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// newTestHandlers wires a GoalService against an in-memory store and mounts
// the HTTP routes. Agents are seeded so goal creation passes existence checks.
func newTestHandlers(t *testing.T) (*Handlers, *store.Store, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	bus := events.NewBus()
	gs := service.NewGoalService(st, bus)

	rt, err := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt-test", MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	agent, err := service.NewAgentService(st, events.NewBus()).Create(ctx, service.Agent{Name: "A", RuntimeID: rt.ID, MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	dom, err := service.NewDomainService(st, events.NewBus()).Create(ctx, service.Domain{Name: "handler-test", GitURL: "https://example.com/handler.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}

	h := &Handlers{Goal: gs}
	mux := http.NewServeMux()
	h.Mount(mux)

	for _, title := range []string{"oldest", "middle", "newest"} {
		if _, err := gs.Create(ctx, service.Goal{Title: title, AssigneeType: "agent", AssigneeID: agent.ID, Status: "active", DomainID: dom.ID}); err != nil {
			t.Fatalf("create goal %q: %v", title, err)
		}
	}
	return h, st, agent.ID
}

func get(t *testing.T, mux *http.ServeMux, path string) (*httptest.ResponseRecorder, []service.Goal) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var goals []service.Goal
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &goals); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return rec, goals
}

// TestListGoalsLimit: GET /goals returns all by default; ?limit=N truncates
// to the N most recent; malformed limit values are rejected with 400.
func TestListGoalsLimit(t *testing.T) {
	h, _, _ := newTestHandlers(t)
	mux := http.NewServeMux()
	h.Mount(mux)

	rec, all := get(t, mux, "/goals")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /goals: status %d, body %s", rec.Code, rec.Body.String())
	}
	if len(all) != 3 {
		t.Fatalf("GET /goals: expected 3 goals, got %d", len(all))
	}
	if all[0].Title != "newest" {
		t.Fatalf("expected newest-first, got first=%q", all[0].Title)
	}

	rec, limited := get(t, mux, "/goals?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /goals?limit=2: status %d, body %s", rec.Code, rec.Body.String())
	}
	if len(limited) != 2 {
		t.Fatalf("GET /goals?limit=2: expected 2 goals, got %d", len(limited))
	}
	if limited[0].Title != "newest" || limited[1].Title != "middle" {
		t.Fatalf("limit=2: expected [newest middle], got %q", []string{limited[0].Title, limited[1].Title})
	}

	for _, bad := range []string{"/goals?limit=abc", "/goals?limit=0", "/goals?limit=-1"} {
		rec, _ = get(t, mux, bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: expected 400, got %d", bad, rec.Code)
		}
	}
}
