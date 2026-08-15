package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/store"
)

// Runtime is a launch spec for a protocol-speaking agent. transport selects
// how the daemon connects (stdio spawns executable+args; ws/tcp dials
// endpoint). provider selects which backend speaks the wire protocol
// (acp|jsonl|jsonrpc). See DESIGN.md
type Runtime struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"` // stdio|ws|tcp
	Provider   string            `json:"provider"`  // acp|jsonl|jsonrpc → which backend
	Executable string            `json:"executable"`
	Args       []string          `json:"args"`
	Endpoint   string            `json:"endpoint"`
	Env        map[string]string `json:"env"`
	// AgentworkURL is the advertised platform base URL for THIS runtime
	// (remote agents need the daemon's public address); '' = the platform
	// default (http://127.0.0.1:<listen port>).
	AgentworkURL string `json:"agentwork_url"`
	CreatedAt    string `json:"created_at"`
}

func validProvider(p string) bool {
	return p == "acp" || p == "jsonl" || p == "jsonrpc"
}

type RuntimeService struct{ st *store.Store }

func NewRuntimeService(st *store.Store) *RuntimeService { return &RuntimeService{st: st} }

func (s *RuntimeService) Create(ctx context.Context, r Runtime) (*Runtime, error) {
	if r.Name == "" {
		return nil, NewValidationError("name is required")
	}
	if r.Transport == "" {
		r.Transport = "stdio"
	}
	switch r.Transport {
	case "stdio":
		if r.Executable == "" {
			return nil, NewValidationError("executable is required for stdio transport")
		}
	case "ws", "tcp":
		if r.Endpoint == "" {
			return nil, NewValidationError("endpoint is required for ws/tcp transport")
		}
	default:
		return nil, NewValidationError("transport must be stdio, ws, or tcp")
	}
	if r.Args == nil {
		r.Args = []string{}
	}
	if r.Env == nil {
		r.Env = map[string]string{}
	}
	if r.Provider == "" {
		r.Provider = "acp"
	}
	if !validProvider(r.Provider) {
		return nil, NewValidationError("provider must be acp, jsonl, or jsonrpc")
	}
	r.ID = newID()
	r.CreatedAt = now()
	argsJSON, _ := json.Marshal(r.Args)
	envJSON, _ := json.Marshal(r.Env)
	_, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,transport,provider,executable,args,endpoint,env,agentwork_url,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.Transport, r.Provider, r.Executable, string(argsJSON), r.Endpoint, string(envJSON), r.AgentworkURL, r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert runtime: %w", err)
	}
	return &r, nil
}

func (s *RuntimeService) List(ctx context.Context) ([]Runtime, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,transport,provider,executable,args,endpoint,env,agentwork_url,created_at FROM runtime ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Runtime{}
	for rows.Next() {
		var r Runtime
		var argsJSON, envJSON string
		if err := rows.Scan(&r.ID, &r.Name, &r.Transport, &r.Provider, &r.Executable, &argsJSON, &r.Endpoint, &envJSON, &r.AgentworkURL, &r.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(argsJSON), &r.Args)
		_ = json.Unmarshal([]byte(envJSON), &r.Env)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *RuntimeService) Get(ctx context.Context, id string) (*Runtime, error) {
	var r Runtime
	var argsJSON, envJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,transport,provider,executable,args,endpoint,env,agentwork_url,created_at FROM runtime WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Transport, &r.Provider, &r.Executable, &argsJSON, &r.Endpoint, &envJSON, &r.AgentworkURL, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(argsJSON), &r.Args)
	_ = json.Unmarshal([]byte(envJSON), &r.Env)
	return &r, nil
}

// GetByEndpoint returns the runtime bound to the given ws/tcp endpoint, or
// ErrNotFound. The endpoint is the identity of a remote (ws/tcp) runtime, so
// this is the dedup lookup behind GetOrCreate.
func (s *RuntimeService) GetByEndpoint(ctx context.Context, endpoint string) (*Runtime, error) {
	var r Runtime
	var argsJSON, envJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,transport,provider,executable,args,endpoint,env,agentwork_url,created_at FROM runtime WHERE endpoint=? LIMIT 1`, endpoint).
		Scan(&r.ID, &r.Name, &r.Transport, &r.Provider, &r.Executable, &argsJSON, &r.Endpoint, &envJSON, &r.AgentworkURL, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(argsJSON), &r.Args)
	_ = json.Unmarshal([]byte(envJSON), &r.Env)
	return &r, nil
}

// GetOrCreate returns the runtime bound to r.Endpoint, creating it (with
// ws/acp defaults) if none exists. Dedup key is the endpoint — the identity of
// an "acp ws" runtime, so selecting the same ws address repeatedly never
// inserts a second row. It backs POST /agents when the caller has no runtime
// entry point and passes the ws address inline.
func (s *RuntimeService) GetOrCreate(ctx context.Context, r Runtime) (*Runtime, error) {
	if r.Endpoint == "" {
		return nil, NewValidationError("runtime.endpoint is required")
	}
	if r.Transport == "" {
		r.Transport = "ws"
	}
	if r.Provider == "" {
		r.Provider = "acp"
	}
	if r.Name == "" {
		r.Name = deriveRuntimeName(r.Endpoint)
	}
	existing, err := s.GetByEndpoint(ctx, r.Endpoint)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return s.Create(ctx, r)
}

// deriveRuntimeName builds a display name from a ws endpoint when the caller
// doesn't supply one: strip the scheme and any trailing slash so
// "ws://127.0.0.1:8787/" becomes "127.0.0.1:8787".
func deriveRuntimeName(endpoint string) string {
	name := endpoint
	name = strings.TrimPrefix(name, "wss://")
	name = strings.TrimPrefix(name, "ws://")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return endpoint
	}
	return name
}

func (s *RuntimeService) Delete(ctx context.Context, id string) error {
	// Refuse if agents still reference this runtime. Cascading would silently
	// orphan agents; the caller should delete or reassign them first.
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE runtime_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check agents: %w", err)
	}
	if n > 0 {
		return NewValidationError(fmt.Sprintf("runtime %s has %d agent(s); delete or reassign them first", id, n))
	}
	_, err := s.st.DB().ExecContext(ctx, `DELETE FROM runtime WHERE id=?`, id)
	return err
}
