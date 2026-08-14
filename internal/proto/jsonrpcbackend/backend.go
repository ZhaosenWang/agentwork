// Package jsonrpcbackend is a stub for the bidirectional JSON-RPC 2.0 protocol
// used by codex (`app-server --listen stdio`). See DESIGN.md
//
// NOT IMPLEMENTED for MVP. Wire = implementing the pending-request map,
// server→client notification handling, `turn/completed` detection with
// completedTurnIDs dedup, and the resume/rejected-session flow (per
// multica server/pkg/agent/codex.go). The acp backend is the only wired
// protocol today.
package jsonrpcbackend

import (
	"context"

	"github.com/eushing/agentwork/internal/proto"
)

type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Execute(ctx context.Context, spec proto.ExecuteSpec) (*proto.Run, error) {
	events := make(chan proto.Event)
	results := make(chan proto.Result, 1)
	go func() {
		defer close(events)
		defer close(results)
		results <- proto.Result{Status: proto.StatusFailed, Output: "jsonrpc backend not implemented yet"}
	}()
	return &proto.Run{Events: events, Result: results}, nil
}
