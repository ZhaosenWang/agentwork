// agentwork-daemon — task-driven multi-runtime agent management platform.
//
// Single binary: HTTP server + embedded daemon. MVP runs everything in one
// process; the daemon can be split out later without changing the store or
// runtime abstractions. The agent-side CLI is a separate binary,
// agentwork-cli; the daemon injects its directory into the agent subprocess
// PATH so the agent can call back.
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

	taskSvc := service.NewTaskService(st, bus)

	// Daemon manages long-lived ACP server subprocesses per agent and
	// claims task_queue rows. Embedded in this process for MVP.
	d := daemon.New(st, bus, *addr, taskSvc)
	go func() {
		if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("daemon: %v", err)
		}
	}()

	srv := server.New(st, bus, d)
	if err := srv.ListenAndServe(ctx, *addr); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("server: %v", err)
	}
}
