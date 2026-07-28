// Package runtime wires a runtime definition (transport + launch params) to a
// live ACP session. It is the transport layer: given a runtime row and the
// agent/task environment, it opens a connection and returns an [*acp.Session]
// ready for Initialize/Prompt. "定义即运行" — the runtime row is the complete
// launch spec; no extra configuration.
//
// Transports:
//
//   - stdio: spawn executable+args as a subprocess, talk ACP over stdin/stdout.
//   - ws:    dial a WebSocket endpoint, talk ACP over the message stream.
//   - tcp:   dial a TCP endpoint, talk ACP over the connection.
//
// All transports speak ACP (JSON-RPC 2.0). The protocol field on the runtime
// row is reserved for future non-ACP runtimes; today it is ignored.
//
// Each call to Launch produces an independent, per-task connection. The
// caller (daemon) is responsible for calling Session.Close when the task
// finishes.
package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/gorilla/websocket"
)

// Spec is the launch spec extracted from a runtime row. Keeping it separate
// from service.Runtime lets this package avoid importing service (and its
// store/events deps); the daemon adapts service.Runtime → Spec.
type Spec struct {
	Transport  string            // stdio|ws|tcp
	Executable string            // stdio only
	Args       []string          // stdio only
	Endpoint   string            // ws/tcp only
	Env        map[string]string // stdio only (layered onto the passed env)
}

// Launch opens a connection per the spec and returns an ACP session over it.
// taskEnv is the base environment (os.Environ()-style "KEY=VALUE" strings);
// for stdio the spec's Env is layered on top before spawning.
func Launch(ctx context.Context, spec Spec, taskEnv []string) (*acp.Session, error) {
	switch spec.Transport {
	case "stdio", "":
		return launchStdio(ctx, spec, taskEnv)
	case "ws":
		return launchWS(ctx, spec.Endpoint)
	case "tcp":
		return launchTCP(ctx, spec.Endpoint)
	default:
		return nil, fmt.Errorf("runtime: unknown transport %q", spec.Transport)
	}
}

// launchStdio spawns the executable and talks ACP over stdin/stdout.
func launchStdio(ctx context.Context, spec Spec, taskEnv []string) (*acp.Session, error) {
	if spec.Executable == "" {
		return nil, fmt.Errorf("runtime: stdio transport requires executable")
	}
	env := taskEnv
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("runtime stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("runtime stdio: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runtime stdio: start %q: %w", spec.Executable, err)
	}
	closeFn := func() error {
		if wc, ok := stdin.(io.WriteCloser); ok {
			_ = wc.Close()
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return nil
	}
	return acp.NewSession(stdout, stdin, closeFn), nil
}

// wsRW adapts a gorilla/websocket.Conn to an io.Reader/io.Writer pair for
// acp.NewSession. ACP over WebSocket uses one JSON-RPC message per text
// frame. A background goroutine reads frames into a pipe that acp's reader
// scans line-by-line; writes send one frame per message.
type wsRW struct {
	conn  *websocket.Conn
	pr    *io.PipeReader
	pw    *io.PipeWriter
	write *sync.Mutex
}

func launchWS(ctx context.Context, endpoint string) (*acp.Session, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("runtime ws: endpoint is required")
	}
	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("runtime ws: dial %q: %w", endpoint, err)
	}
	pr, pw := io.Pipe()
	rw := &wsRW{conn: conn, pr: pr, pw: pw, write: new(sync.Mutex)}
	// Pump text frames into the pipe so acp's bufio.Scanner can read them.
	// ACP messages are newline-delimited JSON; over ws each text frame is
	// one message, so append a newline to keep the scanner happy.
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(append(data, '\n')); err != nil {
				return
			}
		}
	}()
	closeFn := func() error {
		rw.write.Lock()
		defer rw.write.Unlock()
		_ = conn.Close()
		_ = pw.Close()
		return nil
	}
	return acp.NewSession(rw.pr, &wsWriter{rw: rw}, closeFn), nil
}

type wsWriter struct{ rw *wsRW }

func (w *wsWriter) Write(p []byte) (int, error) {
	w.rw.write.Lock()
	defer w.rw.write.Unlock()
	// acp writes one message + '\n' per Write call. Strip the trailing
	// newline: each Write sends one text frame.
	msg := p
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	if err := w.rw.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		return 0, err
	}
	return len(p), nil
}

// launchTCP dials a TCP endpoint and talks ACP over the connection.
// ACP over TCP uses newline-delimited JSON, same as stdio.
func launchTCP(ctx context.Context, endpoint string) (*acp.Session, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("runtime tcp: endpoint is required")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("runtime tcp: dial %q: %w", endpoint, err)
	}
	return acp.NewSession(conn, conn, conn.Close), nil
}
