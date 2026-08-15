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
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/mcp"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/server/handler"
	"github.com/eushing/agentwork/internal/server/ws"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"

	"github.com/gorilla/websocket"
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
	machineSvc := service.NewMachineService(s.st)
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
		Daemon:   s.d,
		Machines: machineSvc,
		Skills:   service.NewSkillService(s.st),
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
	// The agentwork CLI's link (CLI 分支 Phase 1): a JSON-RPC 2.0 over
	// WebSocket connection carrying machine registration, heartbeats, and
	// probe reports (run dispatch/config push land in later phases).
	// Optional token: app_settings platform.worker_token — empty = no
	// auth (single-user local default).
	connectUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true }, // single-user local
	}
	mux.HandleFunc("GET /connect", func(w http.ResponseWriter, r *http.Request) {
		if want, err := settingsSvc.Get(r.Context(), "platform.worker_token"); err == nil && want != "" && r.URL.Query().Get("token") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := connectUpgrader.Upgrade(w, r, nil)
		if err != nil {
			logging.Infof("connect: upgrade: %v", err)
			return
		}
		peer := link.NewPeer(conn)
		// The peer binds to its machine on register; on link death the
		// daemon drops it (dispatches then fail-fast with "machine offline").
		registeredMachineID := ""
		defer func() {
			if registeredMachineID != "" {
				s.d.UnregisterMachinePeer(registeredMachineID)
			}
		}()
		peer.Handle(link.MethodMachineRegister, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.RegisterParams
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			if err := machineSvc.Register(ctx, service.Machine{ID: p.MachineID, Name: p.Name, Hostname: p.Hostname, Version: p.Version}, service.MarshalProbedCLIs(p.CLIs)); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			// Phase 2: each probed CLI becomes an executable runtime row
			// owned by this machine — runs on it dispatch over the link.
			if err := machineSvc.UpsertProbeRuntimes(ctx, p.MachineID, p.Name, p.CLIs); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			// Bind the live peer: dispatched runs for this machine's
			// runtimes flow over THIS link.
			s.d.RegisterMachinePeer(p.MachineID, peer)
			registeredMachineID = p.MachineID
			// Full skills sync (Phase 4): offline edits land on reconnect.
			go s.d.PushMachineSkills(context.Background(), p.MachineID)
			logging.Infof("connect: machine %q (%s) registered, %d agent CLI(s) probed", p.Name, p.Hostname, len(p.CLIs))
			return link.RegisterResult{OK: true}, nil
		})
		peer.Handle(link.MethodRunClaimed, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.RunClaimedParams
			if err := json.Unmarshal(raw, &p); err != nil || p.RunID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "run_id is required"}
			}
			return nil, s.d.IngestRunClaimed(ctx, p)
		})
		peer.Handle(link.MethodRunEventBatch, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.RunEventBatchParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "invalid event batch"}
			}
			return nil, s.d.IngestRunEvents(ctx, p)
		})
		peer.Handle(link.MethodRunFinished, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.RunFinishedParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "invalid finish report"}
			}
			return nil, s.d.IngestRunFinished(ctx, p)
		})
		peer.Handle(link.MethodMachineHeartbeat, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.HeartbeatParams
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			if err := machineSvc.Heartbeat(ctx, p.MachineID); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return nil, nil // notification — no reply
		})
		peer.Handle(link.MethodMachineProbeUpdate, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.ProbeUpdateParams
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			if err := machineSvc.UpdateProbe(ctx, p.MachineID, service.MarshalProbedCLIs(p.CLIs)); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			logging.Infof("connect: machine %s probe update: %d agent CLI(s)", p.MachineID, len(p.CLIs))
			return link.RegisterResult{OK: true}, nil
		})
		peer.Wait()
	})
	// The agent rpc (CLI 分支 Phase 2): one-shot JSON-RPC over WS — the
	// agent's collaboration commands dial this per invocation, carrying the
	// per-run token in params. The token is the ONLY identity: it resolves
	// to the run's (goal, agent, role), and self-reported ids are ignored.
	mux.HandleFunc("GET /rpc", func(w http.ResponseWriter, r *http.Request) {
		conn, err := connectUpgrader.Upgrade(w, r, nil)
		if err != nil {
			logging.Infof("rpc: upgrade: %v", err)
			return
		}
		peer := link.NewPeer(conn)
		agentSvc := service.NewAgentService(s.st, s.bus)

		resolve := func(raw json.RawMessage) (*service.RunIdentity, *link.RPCError) {
			var t link.RPCToken
			_ = json.Unmarshal(raw, &t)
			id, err := s.runSvc.ResolveRunToken(r.Context(), t.Token)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeAuthDenied, Message: err.Error()}
			}
			return id, nil
		}

		peer.Handle(link.MethodGoalComment, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.GoalCommentParams
			if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.Text) == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "text is required"}
			}
			c, err := s.commentSvc.Create(ctx, service.Comment{
				GoalID: id.GoalID, AuthorType: "agent", AuthorID: id.AgentID,
				Content: p.Text, ParentID: p.ParentID, RunID: id.RunID,
			})
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return map[string]any{"id": c.ID}, nil
		})

		peer.Handle(link.MethodGoalComments, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.GoalCommentsParams
			_ = json.Unmarshal(raw, &p)
			if p.Limit <= 0 {
				p.Limit = 50
			}
			out, err := s.commentSvc.ListAfter(ctx, id.GoalID, p.After, p.Limit)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return out, nil
		})

		peer.Handle(link.MethodGoalList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.goalSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodAgentList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := agentSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodSquadList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.squadSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodSubGoalList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.goalSvc.ListSubGoals(ctx, id.GoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodSubGoalCreate, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalCreateParams
			if err := json.Unmarshal(raw, &p); err != nil || p.Title == "" || p.AssigneeID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "title and assignee_id are required"}
			}
			sg, err := s.goalSvc.CreateSubGoal(ctx, id.GoalID, p.Title, p.Description, p.AssigneeID, p.VerifierID, "agent", id.AgentID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return sg, nil
		})

		peer.Handle(link.MethodSubGoalCancel, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalCancelParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id is required"}
			}
			sg, err := s.goalSvc.CancelSubGoal(ctx, p.SubGoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return sg, nil
		})

		peer.Handle(link.MethodSubGoalVerify, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalVerifyParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" || (p.Verdict != "passed" && p.Verdict != "rejected") {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id and verdict (passed|rejected) are required"}
			}
			// VerifySubGoal enforces that the CALLING run is the verifier run.
			if err := s.goalSvc.VerifySubGoal(ctx, id.RunID, p.Verdict, p.Summary, p.Evidence); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return map[string]any{"ok": true}, nil
		})

		peer.Handle(link.MethodGoalWait, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			states, err := s.goalSvc.WaitChildren(ctx, id.GoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return states, nil
		})

		peer.Handle(link.MethodGoalStats, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.goalSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			counts := map[string]int{}
			for _, g := range all {
				counts[g.Status]++
			}
			return map[string]any{"goal_total": len(all), "goal_by_status": counts}, nil
		})

		peer.Wait()
	})

	// Stale sweep: machines that stopped heartbeating flip offline.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := machineSvc.MarkStale(ctx, time.Now().Add(-90*time.Second))
				if err != nil {
					logging.Infof("connect: machine stale sweep: %v", err)
				} else if n > 0 {
					logging.Infof("connect: %d machine(s) offline", n)
				}
			}
		}
	}()
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
		sw := &statusWriter{ResponseWriter: w}
		mcp.HTTPHandler(exec).ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK // WriteHeader never ran — Go's implicit 200
		}
		// Per-tool-call noise at DEBUG only — an agent working makes dozens
		// of these, and the run's message stream is the info-level truth for
		// what it is doing. The debug line carries the FULL URL the agent
		// actually hit (host + path — which advertised address the runtime
		// reached is exactly what remote-agent debugging needs), the run,
		// and the goal (the FULL goal uuid renders as a titled link in the
		// web log panel) so the handshake sequence
		// initialize → tools/list → tools/call stays traceable.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		runID := exec.RunID
		if len(runID) > 8 {
			runID = runID[:8]
		}
		logging.Debugf("mcp: %s %s://%s%s run=%s goal=%s -> %d", r.Method, scheme, r.Host, r.URL.Path, runID, exec.GoalID, status)
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
	// The domain git check tests UNSAVED form values (create/edit dialogs),
	// so it carries the config in the body rather than a domain id.
	mux.HandleFunc("POST /domains/test", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			GitURL         string `json:"git_url"`
			DefaultBranch  string `json:"default_branch"`
			GitCredentials string `json:"git_credentials"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.d.TestDomainGit(r.Context(), body.GitURL, body.DefaultBranch, body.GitCredentials))
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
