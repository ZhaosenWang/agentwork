package link

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// linkTestServer starts an httptest WS server whose peer handles the given
// methods; the client dials it and both peers are returned.
func linkTestServer(t *testing.T, handlers map[string]Handler) (*Peer, *Peer) {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		peer := NewPeer(conn)
		for m, h := range handlers {
			peer.Handle(m, h)
		}
		peer.Wait()
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := NewPeer(conn) // reader loop starts inside NewPeer
	t.Cleanup(client.Close)
	// The server peer is unreachable from here (created inside the handler) —
	// return nil for it; tests exercise the wire through the client.
	return client, nil
}

func TestPeerCallRoundtrip(t *testing.T) {
	client, _ := linkTestServer(t, map[string]Handler{
		"test.echo": func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
			var in map[string]string
			_ = json.Unmarshal(params, &in)
			return map[string]any{"got": in["msg"]}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out struct {
		Got string `json:"got"`
	}
	if err := client.Call(ctx, "test.echo", map[string]string{"msg": "hi"}, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Got != "hi" {
		t.Fatalf("expected echo hi, got %+v", out)
	}
}

func TestPeerUnknownMethod(t *testing.T) {
	client, _ := linkTestServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Call(ctx, "nope.missing", nil, nil)
	if err == nil {
		t.Fatalf("expected method-not-found error")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok || rpcErr.Code != CodeMethodNotFnd {
		t.Fatalf("expected -32601 RPCError, got %T %v", err, err)
	}
}

func TestPeerNotifyNoReply(t *testing.T) {
	got := make(chan struct{})
	client, _ := linkTestServer(t, map[string]Handler{
		"test.ping": func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
			close(got)
			return nil, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Notify(ctx, "test.ping", nil); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatalf("notification never dispatched")
	}
}

func TestPeerHandlerError(t *testing.T) {
	client, _ := linkTestServer(t, map[string]Handler{
		"test.fail": func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
			return nil, &RPCError{Code: CodeAuthDenied, Message: "denied"}
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Call(ctx, "test.fail", nil, nil)
	rpcErr, ok := err.(*RPCError)
	if !ok || rpcErr.Code != CodeAuthDenied || rpcErr.Message != "denied" {
		t.Fatalf("expected auth-denied RPCError, got %T %v", err, err)
	}
}
