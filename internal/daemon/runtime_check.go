package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/runtime"
)

// RuntimeTestResult is the outcome of a runtime connectivity check (POST
// /runtimes/{id}/test). OK=true means the transport opened and the protocol
// handshake completed — the runtime can execute runs.
type RuntimeTestResult struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
	Details   string `json:"details,omitempty"`
}

// testTimeout bounds the runtime check — a hanging executable must not block
// the HTTP request.
const testTimeout = 10 * time.Second

// TestRuntime verifies a runtime can connect and speak its protocol. It opens
// the transport, performs the protocol handshake (ACP: initialize + session/new
// + session/close), and tears down — no task is executed. This lets the owner
// catch bad configs (wrong path, wrong args, protocol mismatch, unreachable
// endpoint) right after creating a runtime, instead of discovering them in a
// failed run after assigning a goal.
func (d *Daemon) TestRuntime(ctx context.Context, runtimeID string) (*RuntimeTestResult, error) {
	var transport, provider, execPath, argsJSON, endpoint, envJSON string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT transport, provider, executable, args, endpoint, env FROM runtime WHERE id=?`, runtimeID).
		Scan(&transport, &provider, &execPath, &argsJSON, &endpoint, &envJSON)
	if err != nil {
		return nil, fmt.Errorf("load runtime: %w", err)
	}

	start := time.Now()
	res := &RuntimeTestResult{}
	defer func() { res.LatencyMs = time.Since(start).Milliseconds() }()

	if provider == "jsonl" || provider == "jsonrpc" {
		res.Error = fmt.Sprintf("provider %q is not implemented yet", provider)
		return res, nil
	}
	if provider != "acp" {
		res.Error = fmt.Sprintf("unsupported provider %q", provider)
		return res, nil
	}

	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	var args []string
	_ = json.Unmarshal([]byte(argsJSON), &args)
	var env map[string]string
	_ = json.Unmarshal([]byte(envJSON), &env)

	spec := runtime.Spec{
		Transport:  transport,
		Executable: execPath,
		Args:       args,
		Endpoint:   endpoint,
		Env:        env,
	}
	conn, err := runtime.Open(ctx, spec, os.Environ())
	if err != nil {
		res.Error = fmt.Sprintf("open transport: %v", err)
		return res, nil
	}
	defer conn.Close()

	sess := acp.NewSession(conn.R, conn.W, conn.Close)
	initResp, err := sess.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1})
	if err != nil {
		res.Error = proto.AppendStderr("initialize: "+err.Error(), conn.Stderr)
		return res, nil
	}
	if _, err := sess.NewSession(ctx, acp.NewSessionRequest{}); err != nil {
		res.Error = proto.AppendStderr("new session: "+err.Error(), conn.Stderr)
		return res, nil
	}
	_ = sess.CloseSession(ctx)

	res.OK = true
	if initResp != nil && initResp.AgentInfo != nil {
		res.Details = fmt.Sprintf("%s %s", initResp.AgentInfo.Name, initResp.AgentInfo.Version)
	}
	return res, nil
}
