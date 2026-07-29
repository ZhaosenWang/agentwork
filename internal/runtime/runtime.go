// Package runtime is the transport layer: given a runtime spec and the
// agent/run environment, it opens a connection and returns a [proto.Conn] —
// a bare read/write pair plus a close fn. It speaks NO protocol; the
// [internal/proto] Backend layered on top does (ACP/JSONL/JSON-RPC).
// "定义即运行" — the runtime row is the complete transport spec.
//
// Transports:
//   - stdio: spawn executable+args as a subprocess, talk over stdin/stdout.
//   - ws:    dial a WebSocket endpoint, talk over the message stream.
//   - tcp:   dial a TCP endpoint, talk over the connection.
//
// Each call to Open produces an independent, per-run connection. The caller
// (daemon) closes it when the run finishes.
package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"

	"github.com/eushing/agentwork/internal/proto"
	"github.com/gorilla/websocket"
)

// Spec is the transport spec extracted from a runtime row. Keeping it separate
// from service.Runtime lets this package avoid importing service (and its
// store/events deps); the daemon adapts service.Runtime → Spec.
type Spec struct {
	Transport  string            // stdio|ws|tcp
	Executable string            // stdio only
	Args       []string          // stdio only
	Endpoint   string            // ws/tcp only
	Env        map[string]string // stdio only (layered onto the passed env)
}

// Open opens a transport connection per the spec and returns a bare Conn.
// taskEnv is the base environment (os.Environ()-style "KEY=VALUE" strings);
// for stdio the spec's Env is layered on top before spawning.
func Open(ctx context.Context, spec Spec, taskEnv []string) (proto.Conn, error) {
	switch spec.Transport {
	case "stdio", "":
		return openStdio(ctx, spec, taskEnv)
	case "ws":
		return openWS(ctx, spec.Endpoint)
	case "tcp":
		return openTCP(ctx, spec.Endpoint)
	default:
		return proto.Conn{}, fmt.Errorf("runtime: unknown transport %q", spec.Transport)
	}
}

// openStdio spawns the executable and exposes stdin/stdout as the Conn's R/W.
func openStdio(ctx context.Context, spec Spec, taskEnv []string) (proto.Conn, error) {
	if spec.Executable == "" {
		return proto.Conn{}, fmt.Errorf("runtime: stdio transport requires executable")
	}
	env := taskEnv
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return proto.Conn{}, fmt.Errorf("runtime stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return proto.Conn{}, fmt.Errorf("runtime stdio: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return proto.Conn{}, fmt.Errorf("runtime stdio: start %q: %w", spec.Executable, err)
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
	// Expose stderr as a reader over the captured buffer. The backend reads it
	// lazily on failure for diagnostics; it's a bytes.Buffer so concurrent
	// reads/writes on the buffer are NOT safe, but the run is single-writer
	// (the subprocess) single-reader (the backend, post-turn) so it's fine.
	return proto.Conn{R: stdout, W: stdin, Close: closeFn, Stderr: &stderrBuf}, nil
}

// wsRW adapts a gorilla/websocket.Conn to an io.Reader/io.Writer pair. A
// background goroutine reads frames into a pipe that the backend scans; writes
// send one frame per message.
type wsRW struct {
	conn  *websocket.Conn
	pr    *io.PipeReader
	pw    *io.PipeWriter
	write *sync.Mutex
}

func openWS(ctx context.Context, endpoint string) (proto.Conn, error) {
	if endpoint == "" {
		return proto.Conn{}, fmt.Errorf("runtime ws: endpoint is required")
	}
	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return proto.Conn{}, fmt.Errorf("runtime ws: dial %q: %w", endpoint, err)
	}
	pr, pw := io.Pipe()
	rw := &wsRW{conn: conn, pr: pr, pw: pw, write: new(sync.Mutex)}
	// Pump text frames into the pipe so a line-scanning backend can read them.
	// Over ws each text frame is one JSON-RPC message; append a newline so a
	// newline-delimited scanner stays happy.
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
	return proto.Conn{R: rw.pr, W: &wsWriter{rw: rw}, Close: closeFn}, nil
}

type wsWriter struct{ rw *wsRW }

func (w *wsWriter) Write(p []byte) (int, error) {
	w.rw.write.Lock()
	defer w.rw.write.Unlock()
	// The ACP backend writes one message + '\n' per Write call. Over ws each
	// Write sends one text frame; strip the trailing newline.
	msg := p
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	if err := w.rw.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		return 0, err
	}
	return len(p), nil
}

// openTCP dials a TCP endpoint. ACP over TCP uses newline-delimited JSON.
func openTCP(ctx context.Context, endpoint string) (proto.Conn, error) {
	if endpoint == "" {
		return proto.Conn{}, fmt.Errorf("runtime tcp: endpoint is required")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return proto.Conn{}, fmt.Errorf("runtime tcp: dial %q: %w", endpoint, err)
	}
	return proto.Conn{R: conn, W: conn, Close: conn.Close}, nil
}