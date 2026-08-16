package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// cliVersion is the agentwork CLI build version, reported in register.
// var (not const): the release build stamps the real version via
// -ldflags "-X <pkg>.cliVersion=$AGENTWORK_COMPILE_VERSION" (build.sh) so
// the CLI and the daemon ship one shared version — the register-time
// version check depends on both being built from the same stamp.
var cliVersion = "0.0.1-beta.1"

// stateFile is where connect persists the machine identity + last
// connection state (status reads it — no IPC needed in Phase 1).
func stateFile() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "agentwork-cli-state.json"
	}
	return filepath.Join(dir, ".agentwork", "agentwork-cli-state.json")
}

type cliState struct {
	MachineID   string    `json:"machine_id"`
	Name        string    `json:"name"`
	Server      string    `json:"server"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
	ProbedCLIs  []link.ProbeCLI `json:"probed_clis,omitempty"`
}

// loadState reads the persisted state (missing file = fresh machine).
func loadState() (cliState, error) {
	var s cliState
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return cliState{}, err
	}
	return s, nil
}

// saveState persists the state file (best-effort; status tolerates a
// missing file).
func saveState(s cliState) {
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.MkdirAll(filepath.Dir(stateFile()), 0o755)
	_ = os.WriteFile(stateFile(), b, 0o644)
}

// connectCmd runs `agentwork connect`: dial the daemon's /connect link,
// register this machine (probing its agent CLIs), then heartbeat until
// interrupted. Reconnects with exponential backoff.
func connectCmd(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	server := fs.String("server", defaultServerURL, "agentwork-daemon address")
	token := fs.String("token", "", "register token (daemon-side platform.worker_token; empty = none)")
	name := fs.String("name", "", "machine display name (default: hostname)")
	fs.Parse(args)

	hostname, _ := os.Hostname()
	if *name == "" {
		*name = hostname
	}

	st, err := loadState()
	if err != nil || st.MachineID == "" {
		st = cliState{MachineID: uuid.NewString(), Name: *name}
	}
	st.Server = *server
	st.Name = *name
	saveState(st)

	wsURL := connectWSURL(*server, *token)
	fmt.Printf("agentwork connect: server=%s machine=%q (%s)\n", *server, st.Name, st.MachineID)

	// One cancellable ctx for the whole session: Ctrl+C / SIGTERM aborts
	// dialing, in-flight calls, heartbeats AND the reconnect backoff —
	// the process must exit promptly, never strand on a stuck link.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backoff := time.Second
	for {
		if err := runLink(ctx, wsURL, st, *name, hostname); err != nil {
			fmt.Printf("connect: %v — reconnecting in %s\n", err, backoff)
		}
		select {
		case <-ctx.Done():
			fmt.Println("disconnected")
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// runLink holds one connection: register → heartbeat loop → drain until
// the link dies or ctx is cancelled. Returns the exit error for the
// reconnect loop (nil on ctx cancellation — that is a clean shutdown).
func runLink(ctx context.Context, wsURL string, st cliState, name, hostname string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	peer := link.NewPeer(conn)
	defer peer.Close()
	// Phase 2: the machine executes dispatched runs over THIS link — wire
	// the executor's handlers, and cancel everything in flight when the
	// link dies (the platform reclaims them via RecoverStuckRunning).
	exec := newExecutor(peer, st.Server)
	defer exec.shutdown()
	peer.Handle(link.MethodRunDispatch, exec.handleDispatch)
	peer.Handle(link.MethodRunCancel, exec.handleCancel)
	// config.push (Phase 4): land the agent's skill packages in the
	// platform-managed staging dir (~/.agentwork/skills/<agentID>/...) —
	// the executor copies them into each run's workdir at spawn as
	// PROJECT-level skills (original names, the user's own global skills
	// untouched). The agent's whole staging dir is rebuilt from scratch, so
	// removals propagate.
	peer.Handle(link.MethodConfigPush, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
		var p link.ConfigPushParams
		if err := json.Unmarshal(raw, &p); err != nil || p.AgentID == "" {
			return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "agent_id is required"}
		}
		res := link.ConfigPushResult{}
		// The agent-level persona (system prompt) rides AGENTS.md in the
		// per-agent config dir — the runtime's profile resolver loads it
		// natively from the workdir; the per-run role contract stays in the
		// prompt.
		if err := writeAgentProfile(p.AgentID, p.SystemPrompt); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("AGENTS.md: %v", err))
		}
		root := machineSkillRoot(p.AgentID)
		_ = os.RemoveAll(root)
		for _, sk := range p.Skills {
			if sk.Name == "" || sk.Name == "." || sk.Name == ".." || strings.ContainsAny(sk.Name, "/\\") {
				res.Errors = append(res.Errors, fmt.Sprintf("%q: invalid skill name", sk.Name))
				continue
			}
			target := filepath.Join(root, sk.Name)
			bad := false
			for _, f := range sk.Files {
				if filepath.IsAbs(f.Path) || f.Path == ".." || strings.HasPrefix(f.Path, "../") {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: invalid path %q", sk.Name, f.Path))
					bad = true
					break
				}
				path := filepath.Join(target, filepath.FromSlash(f.Path))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", sk.Name, err))
					bad = true
					break
				}
				if err := os.WriteFile(path, []byte(f.Content), 0o644); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", sk.Name, err))
					bad = true
					break
				}
			}
			if !bad {
				res.Written = append(res.Written, sk.Name)
			}
		}
		fmt.Printf("config.push: installed %d skill(s) for agent %s into %s\n", len(res.Written), p.AgentID, root)
		return res, nil
	})

	clis := probeCLIs(ctx)
	fmt.Printf("probed %d agent CLI(s):", len(clis))
	for _, c := range clis {
		fmt.Printf(" %s(%s)", c.Name, c.Version)
	}
	fmt.Println()

	var res link.RegisterResult
	if err := peer.Call(ctx, link.MethodMachineRegister, link.RegisterParams{
		MachineID: st.MachineID,
		Name:      name,
		Hostname:  hostname,
		Version:   cliVersion,
		CLIs:      clis,
	}, &res); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if res.ServerVersion != "" && res.ServerVersion != cliVersion {
		fmt.Printf("warning: daemon v%s vs CLI v%s — protocol drift possible\n", res.ServerVersion, cliVersion)
	}
	fmt.Println("registered")
	flushPendingReports(peer)
	st.ConnectedAt = time.Now()
	st.ProbedCLIs = clis
	saveState(st)

	// Heartbeat every 5s until the link dies or we're interrupted.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if err := peer.Notify(ctx, link.MethodMachineHeartbeat, link.HeartbeatParams{MachineID: st.MachineID}); err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
			st.ConnectedAt = time.Now()
			saveState(st)
		case <-peer.Done():
			return fmt.Errorf("link closed")
		case <-ctx.Done():
			return nil
		}
	}
}

// connectWSURL derives the /connect WebSocket URL from the server flag
// (http(s) → ws(s); bare host:port → ws).
func connectWSURL(server, token string) string {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "ws", Host: server, Path: "/connect"}
	} else {
		switch u.Scheme {
		case "https":
			u.Scheme = "wss"
		default:
			u.Scheme = "ws"
		}
		u.Path = "/connect"
	}
	q := u.Query()
	if token != "" {
		q.Set("token", token)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// statusCmd runs `agentwork status`: the persisted connection state.
func statusCmd(args []string) {
	st, err := loadState()
	if err != nil {
		fmt.Println("status: not connected (no state file — run `agentwork connect` first)")
		return
	}
	status := "offline"
	if !st.ConnectedAt.IsZero() && time.Since(st.ConnectedAt) < 15*time.Second {
		status = "connected"
	}
	fmt.Printf("machine:   %s (%s)\n", st.Name, st.MachineID)
	fmt.Printf("server:    %s\n", st.Server)
	if st.ConnectedAt.IsZero() {
		fmt.Printf("status:    %s (never connected)\n", status)
	} else {
		fmt.Printf("status:    %s (last heartbeat %s)\n", status, st.ConnectedAt.Format(time.RFC3339))
	}
	if len(st.ProbedCLIs) > 0 {
		fmt.Println("agent CLIs:")
		for _, c := range st.ProbedCLIs {
			fmt.Printf("  %s %s\n", c.Name, c.Version)
		}
	} else {
		fmt.Println("agent CLIs: none probed")
	}
}
