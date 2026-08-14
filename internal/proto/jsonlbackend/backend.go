// Package jsonlbackend is a stub for the single-direction JSONL-stream
// protocol used by claude (`--output-format stream-json`) and opencode
// (`run --format json`). See DESIGN.md
//
// NOT IMPLEMENTED for MVP. Execute returns a "not implemented" Result so a
// misconfigured runtime surfaces a clean failure instead of hanging. The acp
// backend is the only wired protocol today. Wiring this = implementing the
// bufio.Scanner loop + step_start/step_finish pairing (fail-closed on EOF,
// per multica §9.2).
package jsonlbackend

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
		results <- proto.Result{Status: proto.StatusFailed, Output: "jsonl backend not implemented yet"}
	}()
	return &proto.Run{Events: events, Result: results}, nil
}
