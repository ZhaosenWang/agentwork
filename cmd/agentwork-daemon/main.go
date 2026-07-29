// agentwork-daemon — task-driven multi-runtime agent management platform.
//
// Single binary: HTTP server + embedded daemon. MVP runs everything in one
// process. The agent-side CLI (agentwork-cli) is a separate binary; the daemon
// injects its directory into the agent subprocess PATH so the agent can call
// back.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/proto/acpbackend"
	"github.com/eushing/agentwork/internal/proto/jsonlbackend"
	"github.com/eushing/agentwork/internal/proto/jsonrpcbackend"
	"github.com/eushing/agentwork/internal/server"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

func main() {
	addr := flag.String("addr", ":7373", "HTTP listen address")
	dbPath := flag.String("db", "", "SQLite path (default ~/.agentwork/agentwork.db)")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus := events.NewBus()

	// Services. wired together explicitly to avoid a constructor-order cycle:
	// GoalService <-> RunService hold cross-references (reconcile enqueues
	// retries/wakes; run finish delegates to reconcile).
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	commentSvc := service.NewCommentService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	commentSvc.SetRunService(runSvc)
	commentSvc.SetGoalService(goalSvc)

	squadSvc := service.NewSquadService(st, bus)
	schedSvc := service.NewScheduleService(st, bus)

	// Protocol backends registered by provider name (runtime.provider selects).
	protoReg := proto.NewRegistry()
	protoReg.Register("acp", acpbackend.New())
	protoReg.Register("jsonl", jsonlbackend.New())
	protoReg.Register("jsonrpc", jsonrpcbackend.New())

	d := daemon.New(st, bus, *addr, protoReg, goalSvc, runSvc, squadSvc, schedSvc)
	go func() {
		if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("daemon: %v", err)
		}
	}()

	srv := server.New(st, bus, d, goalSvc, runSvc, commentSvc, squadSvc, schedSvc)
	if err := srv.ListenAndServe(ctx, *addr); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("server: %v", err)
	}
}