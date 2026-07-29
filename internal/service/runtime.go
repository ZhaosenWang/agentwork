package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eushing/agentwork/internal/store"
)

// Runtime is a launch spec for a protocol-speaking agent. transport selects
// how the daemon connects (stdio spawns executable+args; ws/tcp dials
// endpoint). provider selects which backend speaks the wire protocol
// (acp|jsonl|jsonrpc). See DESIGN.zh.md §6.
type Runtime struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Transport  string            `json:"transport"`  // stdio|ws|tcp
	Provider   string            `json:"provider"`   // acp|jsonl|jsonrpc → which backend
	Executable string            `json:"executable"`
	Args       []string          `json:"args"`
	Endpoint   string            `json:"endpoint"`
	Env        map[string]string `json:"env"`
	CreatedAt  string            `json:"created_at"`
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
		`INSERT INTO runtime (id,name,transport,provider,executable,args,endpoint,env,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.Transport, r.Provider, r.Executable, string(argsJSON), r.Endpoint, string(envJSON), r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert runtime: %w", err)
	}
	return &r, nil
}

func (s *RuntimeService) List(ctx context.Context) ([]Runtime, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,transport,provider,executable,args,endpoint,env,created_at FROM runtime ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runtime
	for rows.Next() {
		var r Runtime
		var argsJSON, envJSON string
		if err := rows.Scan(&r.ID, &r.Name, &r.Transport, &r.Provider, &r.Executable, &argsJSON, &r.Endpoint, &envJSON, &r.CreatedAt); err != nil {
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
		`SELECT id,name,transport,provider,executable,args,endpoint,env,created_at FROM runtime WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Transport, &r.Provider, &r.Executable, &argsJSON, &r.Endpoint, &envJSON, &r.CreatedAt)
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
