package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

func newTestSettings(t *testing.T, token string) *service.SettingsService {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := service.NewSettingsService(st)
	if token != "" {
		_ = svc.Set(context.Background(), "platform.worker_token", token)
	}
	return svc
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7373", true},
		{"localhost:7373", true},
		{"[::1]:7373", true},
		{"0.0.0.0:7373", false},
		{":7373", false},
		{"0.0.0.0:8080", false},
		{"192.168.1.100:7373", false},
		{"10.0.0.1:7373", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	settings := newTestSettings(t, "secret123")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := authMiddleware(settings, next)

	// Correct token in query
	called = false
	req := httptest.NewRequest("GET", "/?token=secret123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("query token: expected 200 + called, got %d called=%v", rec.Code, called)
	}

	// Correct token in Authorization header
	called = false
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("bearer token: expected 200 + called, got %d called=%v", rec.Code, called)
	}

	// Wrong token
	called = false
	req = httptest.NewRequest("GET", "/?token=wrong", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("wrong token: expected 401 + not called, got %d called=%v", rec.Code, called)
	}

	// Missing token
	called = false
	req = httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("missing token: expected 401 + not called, got %d called=%v", rec.Code, called)
	}

	// /rpc is EXEMPT (agent/link channel with app-layer auth)
	called = false
	req = httptest.NewRequest("GET", "/rpc", nil) // no token at all
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("/rpc: exempt path should pass without token, got %d called=%v", rec.Code, called)
	}

	// /connect is NOT exempt (machine connect CLI passes ?token=)
	called = false
	req = httptest.NewRequest("GET", "/connect", nil) // no token
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("/connect: without token should be 401, got %d called=%v", rec.Code, called)
	}
	called = false
	req = httptest.NewRequest("GET", "/connect?token=secret123", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("/connect: with token should pass, got %d called=%v", rec.Code, called)
	}

	// OPTIONS (CORS preflight) is EXEMPT — browsers send it without token
	called = false
	req = httptest.NewRequest(http.MethodOptions, "/api/goals", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("OPTIONS preflight: expected pass without token, got %d called=%v", rec.Code, called)
	}

	// Token rotation: changing the settings takes effect immediately
	_ = settings.Set(context.Background(), "platform.worker_token", "rotated456")
	called = false
	req = httptest.NewRequest("GET", "/?token=secret123", nil) // old token
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("old token after rotation: expected 401, got %d", rec.Code)
	}
	called = false
	req = httptest.NewRequest("GET", "/?token=rotated456", nil) // new token
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("new token after rotation: expected 200, got %d", rec.Code)
	}
}
