package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/gorilla/websocket"
)

// rpcTimeout bounds one agent rpc call (goal.wait parks server-side up to
// its own deadline; the call ctx must outlive it).
const rpcTimeout = 15 * time.Minute

// rpcCall dials the daemon's /rpc endpoint for ONE JSON-RPC request — the
// agent's collaboration commands are one-shot: connect, call, print the
// JSON result, exit. The per-run token rides the environment
// (AGENTWORK_TOKEN), injected by the executor at spawn.
func rpcCall(method string, params any, out any) error {
	serverURL := os.Getenv("AGENTWORK_SERVER_URL")
	if serverURL == "" {
		serverURL = defaultServerURL
	}
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "ws", Host: serverURL, Path: "/rpc"}
	} else {
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		u.Path = "/rpc"
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial rpc: %w", err)
	}
	peer := link.NewPeer(conn)
	defer peer.Close()
	if err := peer.Call(ctx, method, params, out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// rpcToken builds the standard params prefix from the environment.
func rpcToken() link.RPCToken {
	return link.RPCToken{Token: os.Getenv("AGENTWORK_TOKEN")}
}

// rpcPrintJSON marshals a result and prints it — the CLI's native output
// format (agents parse stdout).
func rpcPrintJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail("marshal result: %v", err)
	}
	fmt.Println(string(b))
}
