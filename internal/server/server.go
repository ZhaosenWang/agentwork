// Package server is the HTTP + WebSocket boundary. Routes task/agent/runtime
// CRUD and streams session events to the frontend over WS.
package server

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/issue"
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
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		ws.ServeWS(s.hub, w, r)
	})
	h.Mount(mux)

	go s.hub.Run(ctx)

	srv := &http.Server{Addr: addr, Handler: corsMiddleware(mux)}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("server: listening on %s", addr)
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
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
