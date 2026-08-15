// Package server is the HTTP + WebSocket boundary. Routes task/agent/runtime
// CRUD and streams session events to the frontend over WS.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/mcp"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/server/handler"
	"github.com/eushing/agentwork/internal/server/ws"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

type Server struct {
	st         *store.Store
	bus        *events.Bus
	d          *daemon.Daemon
	hub        *ws.Hub
	goalSvc    *service.GoalService
	runSvc     *service.RunService
	commentSvc *service.CommentService
	squadSvc   *service.SquadService
	schedSvc   *service.ScheduleService
	domainSvc  *service.DomainService
	imConn     *notify.Connector
}

// statusWriter captures the response status for request logging (the MCP
// handshake's health is only visible through which requests return what).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func New(st *store.Store, bus *events.Bus, d *daemon.Daemon, goalSvc *service.GoalService, runSvc *service.RunService, commentSvc *service.CommentService, squadSvc *service.SquadService, schedSvc *service.ScheduleService, domainSvc *service.DomainService, imConn *notify.Connector) *Server {
	return &Server{st: st, bus: bus, d: d, hub: ws.NewHub(bus), goalSvc: goalSvc, runSvc: runSvc, commentSvc: commentSvc, squadSvc: squadSvc, schedSvc: schedSvc, domainSvc: domainSvc, imConn: imConn}
}

// ListenAndServe mounts routes and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	settingsSvc := service.NewSettingsService(s.st)
	h := &handler.Handlers{
		Runtime:  service.NewRuntimeService(s.st),
		Agent:    service.NewAgentService(s.st, s.bus),
		Goal:     s.goalSvc,
		Run:      s.runSvc,
		Comment:  s.commentSvc,
		Squad:    s.squadSvc,
		Schedule: s.schedSvc,
		Domain:   s.domainSvc,
		Settings: settingsSvc,
		IM:       s.imConn,
		// M4-B: the real-time issue triggers (github + gitcode) share the
		// poller's create path (source_ref idempotency makes webhook + poll
		// racing safe). The shared secret lives in app_settings
		// (platform.webhook_secret — shared across providers: one secret
		// configures every repo's webhook on a single-user platform);
		// empty = webhook disabled, polling still covers it.
		IssueWebhooks: map[string]*issue.WebhookHandler{
			"github": issue.NewWebhookHandler("github", s.st, s.d.Poller(),
				func(ctx context.Context) (string, error) { return settingsSvc.Get(ctx, "platform.webhook_secret") }),
			"gitcode": issue.NewWebhookHandler("gitcode", s.st, s.d.Poller(),
				func(ctx context.Context) (string, error) { return settingsSvc.Get(ctx, "platform.webhook_secret") }),
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Logs API: the Web logs panel's history (time/level filtered) and the
	// runtime level knob (persisted — the daemon restores it at startup).
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var after, before *time.Time
		if v := q.Get("after"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				after = &t
			}
		}
		if v := q.Get("before"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				before = &t
			}
		}
		limit := 500
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		minLevel := logging.ParseLevel(q.Get("level"))
		lines, err := logging.ReadLogs(logging.DefaultPath(), after, before, limit, minLevel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"level": logging.GetLevel().String(), "lines": lines})
	})
	mux.HandleFunc("GET /logs/level", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"level": logging.GetLevel().String()})
	})
	mux.HandleFunc("PUT /logs/level", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lv := logging.ParseLevel(body.Level)
		if lv.String() != body.Level {
			http.Error(w, "level must be debug, info, warn, or error", http.StatusBadRequest)
			return
		}
		logging.SetLevel(lv)
		if err := settingsSvc.Set(context.Background(), "logging.level", lv.String()); err != nil {
			logging.Errorf("server: persist log level: %v", err)
		}
		logging.Infof("logging: level set to %s (Web)", lv)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"level": lv.String()})
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		ws.ServeWS(s.hub, w, r)
	})
	// Workspace MCP server per run (DESIGN.md 决策 4-8): the run's executor
	// is registered in the daemon while the run is active; the agent reaches
	// it at /mcp/{runID} (the URL is advertised in session/new mcpServers).
	// streamable HTTP: POST for JSON-RPC, GET for the SSE event stream —
	// opencode subscribes with a GET after initialize; without the GET route
	// it gets 405 and never registers the MCP tools (a live run proved it:
	// three POSTs arrived, the GET was missing, no tools appeared).
	serveMCP := func(w http.ResponseWriter, r *http.Request) {
		exec := s.d.MCPExecutor(r.PathValue("runID"))
		if exec == nil {
			http.Error(w, "no active run with this id", http.StatusNotFound)
			return
		}
		// Method + status logged: the agent's handshake is
		// initialize → tools/list → tools/call — a silent handshake break
		// (e.g. the tools/list step) is otherwise invisible (live: a retry
		// run got 4 requests then never called a tool again).
		sw := &statusWriter{ResponseWriter: w}
		mcp.HTTPHandler(exec).ServeHTTP(sw, r)
		logging.Infof("mcp: %s /mcp/%s -> %d", r.Method, r.PathValue("runID"), sw.status)
	}
	mux.HandleFunc("POST /mcp/{runID}", serveMCP)
	mux.HandleFunc("GET /mcp/{runID}", serveMCP)
	// Human-initiated run stop (决策 4-12): terminates the running run (no
	// attempt consumed, no auto-retry), goal state untouched — recovery is
	// the human's call.
	mux.HandleFunc("POST /goals/{goalID}/runs/{runID}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := s.d.StopRun(r.PathValue("goalID"), r.PathValue("runID")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Runtime connectivity check: opens the transport + does the protocol
	// handshake without executing a task, so the owner can verify a runtime
	// is usable right after creating it — instead of discovering a bad config
	// in a failed run after assigning a goal.
	mux.HandleFunc("POST /runtimes/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.d.TestRuntime(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "runtime not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	h.Mount(mux)

	go s.hub.Run(ctx)

	srv := &http.Server{Addr: addr, Handler: corsMiddleware(mux)}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	logging.Infof("server: listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// corsMiddleware allows the Next.js dev server (localhost:3000) to call the
// API and WS endpoint cross-origin. Single-user local use; no credentialed
// requests, no origin allow-list beyond the dev port.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
