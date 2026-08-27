package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eushing/agentwork/internal/store"
)

// Runtime is the launch spec of one probed agent CLI on one registered
// machine (CLI 分支): the machine spawns it with args (acp_spawn) when a
// run dispatches. The local-transport concepts (transport/provider/
// executable/endpoint) are retired — the daemon never opens a transport.
// The wire protocol is the machine's implementation detail (ACP today; a
// future a2a backend would live in the machine's executor).
type Runtime struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	// MachineID is the registered machine that executes this runtime's
	// runs (transport='agentwork'; '' = local/legacy transports).
	MachineID string `json:"machine_id,omitempty"`
	// Status: active | absent — absent = the machine's latest probe no
	// longer sees this CLI (uninstalled); the claim gate rejects it.
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type RuntimeService struct{ st *store.Store }

func NewRuntimeService(st *store.Store) *RuntimeService { return &RuntimeService{st: st} }

func (s *RuntimeService) Create(ctx context.Context, r Runtime) (*Runtime, error) {
	if r.Name == "" {
		return nil, NewFieldRequiredError("name")
	}
	if r.MachineID == "" {
		return nil, NewFieldRequiredError("machine_id")
	}
	if r.Args == nil {
		r.Args = []string{}
	}
	if r.Env == nil {
		r.Env = map[string]string{}
	}
	r.ID = newID()
	r.CreatedAt = now()
	argsJSON, _ := json.Marshal(r.Args)
	envJSON, _ := json.Marshal(r.Env)
	_, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO runtime (id,name,machine_id,args,env,created_at) VALUES (?,?,?,?,?,?)`,
		r.ID, r.Name, r.MachineID, string(argsJSON), string(envJSON), r.CreatedAt)
	if err != nil {
		if ve := dupNameCodedError(err, CodeRuntimeNameExists, "runtime", r.Name); ve != nil {
			return nil, ve
		}
		return nil, fmt.Errorf("insert runtime: %w", err)
	}
	return &r, nil
}

func (s *RuntimeService) List(ctx context.Context) ([]Runtime, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,machine_id,args,env,status,created_at FROM runtime ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Runtime{}
	for rows.Next() {
		var r Runtime
		var argsJSON, envJSON string
		if err := rows.Scan(&r.ID, &r.Name, &r.MachineID, &argsJSON, &envJSON, &r.Status, &r.CreatedAt); err != nil {
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
		`SELECT id,name,machine_id,args,env,created_at FROM runtime WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.MachineID, &argsJSON, &envJSON, &r.CreatedAt)
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
	// orphan agents; the caller should delete its agents first.
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent WHERE runtime_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check agents: %w", err)
	}
	if n > 0 {
		var name string
		_ = s.st.DB().QueryRowContext(ctx, `SELECT name FROM runtime WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeRuntimeHasAgents,
			fmt.Sprintf("runtime %q has %d agent(s); delete its agents first", name, n),
			map[string]any{"name": name, "count": n})
	}
	_, err := s.st.DB().ExecContext(ctx, `DELETE FROM runtime WHERE id=?`, id)
	return err
}
