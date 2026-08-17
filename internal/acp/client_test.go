package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingReader blocks on Read until `release` is closed, then returns
// (0, err). This lets the test synchronize: the pending call is registered
// BEFORE the write (see Session.request), so once the write fires, failPending
// will find the pending call.
type blockingReader struct {
	err     error
	release chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.release
	return 0, r.err
}

// signalWriter closes `written` on the first Write call. Session.request
// registers the pending call before writing, so once Write fires the pending
// call is already in the map — failPending will find it.
type signalWriter struct {
	once    sync.Once
	written chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.written) })
	return len(p), nil
}

// TestFailPendingSurfacesReadError: a non-EOF read error from the transport
// must appear in the failed call's error message. Without Fix 1, a ws/tcp
// connection drop produces a bare "transport closed" that hides the actual
// cause (websocket close code, TCP reset, …), leaving the run output with
// no diagnostic.
func TestFailPendingSurfacesReadError(t *testing.T) {
	readErr := errors.New("websocket: close 1006 abnormal closure")
	r := &blockingReader{err: readErr, release: make(chan struct{})}
	w := &signalWriter{written: make(chan struct{})}
	sess := NewSession(r, w, func() error { return nil })

	errCh := make(chan error, 1)
	go func() {
		_, err := sess.Initialize(context.Background(), InitializeRequest{ProtocolVersion: 1})
		errCh <- err
	}()

	select {
	case <-w.written:
	case <-time.After(2 * time.Second):
		t.Fatal("Initialize did not write its request in time")
	}

	close(r.release)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "websocket: close 1006") {
			t.Fatalf("error should contain the read error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "transport closed") {
			t.Fatalf("error should mention transport closed, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Initialize did not return after transport error")
	}
}

// TestFailPendingCleanEOF: a clean EOF (no read error) produces the original
// generic message without a spurious error suffix — bufio.Scanner.Err()
// returns nil for io.EOF.
func TestFailPendingCleanEOF(t *testing.T) {
	r := &blockingReader{err: io.EOF, release: make(chan struct{})}
	w := &signalWriter{written: make(chan struct{})}
	sess := NewSession(r, w, func() error { return nil })

	errCh := make(chan error, 1)
	go func() {
		_, err := sess.Initialize(context.Background(), InitializeRequest{ProtocolVersion: 1})
		errCh <- err
	}()

	select {
	case <-w.written:
	case <-time.After(2 * time.Second):
		t.Fatal("Initialize did not write its request in time")
	}

	close(r.release)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "transport closed") {
			t.Fatalf("error should mention transport closed, got: %v", err)
		}
		if strings.Contains(err.Error(), "EOF") {
			t.Fatalf("clean EOF should not add an error suffix, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Initialize did not return after clean EOF")
	}
}

// TestPermissionOutcomeWire pins the outcome to the ACP spec's tagged
// union — the "outcome" discriminant is REQUIRED (live: omitting it made
// opencode read our approvals as rejections; the VS Code client sends
// "selected" and works).
func TestPermissionOutcomeWire(t *testing.T) {
	id := PermissionOptionId("once")
	sel, err := json.Marshal(RequestPermissionOutcome{Outcome: PermissionOutcomeSelected, OptionID: &id})
	if err != nil {
		t.Fatal(err)
	}
	if string(sel) != `{"outcome":"selected","optionId":"once"}` {
		t.Fatalf("selected outcome wire = %s", sel)
	}
	can, err := json.Marshal(RequestPermissionOutcome{Outcome: PermissionOutcomeCancelled})
	if err != nil {
		t.Fatal(err)
	}
	if string(can) != `{"outcome":"cancelled"}` {
		t.Fatalf("cancelled outcome wire = %s", can)
	}
}
