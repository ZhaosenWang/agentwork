package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/store"
)

// Machine is one remote execution host registered via `agentwork connect`
// (CLI 分支 Phase 1). The probed agent CLIs ride ProbedCLIs (raw JSON) —
// they become runtime rows once remote execution lands (Phase 2).
type Machine struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname"`
	Version    string `json:"version"`
	ProbedCLIs string `json:"probed_clis"` // JSON []ProbeCLI
	LastSeenAt string `json:"last_seen_at"`
	Status     string `json:"status"` // connected|offline
	CreatedAt  string `json:"created_at"`
}

// MachineService is the registry behind the /connect link.
type MachineService struct {
	st *store.Store
}

// NewMachineService wires the registry.
func NewMachineService(st *store.Store) *MachineService {
	return &MachineService{st: st}
}

// Register upserts the machine (same machine_id across reconnects) and
// marks it connected with the fresh probe report.
func (s *MachineService) Register(ctx context.Context, m Machine, probedCLIsJSON string) error {
	if m.ID == "" {
		return NewValidationError("machine_id is required")
	}
	ts := now()
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO machine (id,name,hostname,version,probed_clis,last_seen_at,status,created_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name, hostname=excluded.hostname, version=excluded.version,
		   probed_clis=excluded.probed_clis, last_seen_at=excluded.last_seen_at, status='connected'`,
		m.ID, m.Name, m.Hostname, m.Version, probedCLIsJSON, ts, "connected", ts); err != nil {
		return fmt.Errorf("register machine: %w", err)
	}
	return nil
}

// Heartbeat refreshes last_seen_at and re-marks the machine connected
// (a machine that was swept offline comes back without a new register).
func (s *MachineService) Heartbeat(ctx context.Context, machineID string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE machine SET last_seen_at=?, status='connected' WHERE id=?`,
		now(), machineID); err != nil {
		return fmt.Errorf("machine heartbeat: %w", err)
	}
	return nil
}

// UpdateProbe stores a fresh probe report.
func (s *MachineService) UpdateProbe(ctx context.Context, machineID, probedCLIsJSON string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE machine SET probed_clis=?, last_seen_at=? WHERE id=?`,
		probedCLIsJSON, now(), machineID); err != nil {
		return fmt.Errorf("machine probe update: %w", err)
	}
	return nil
}

// List returns every registered machine, most recently seen first.
func (s *MachineService) List(ctx context.Context) ([]Machine, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,hostname,version,probed_clis,last_seen_at,status,created_at
		 FROM machine ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.ID, &m.Name, &m.Hostname, &m.Version, &m.ProbedCLIs, &m.LastSeenAt, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkStale flips connected machines whose last heartbeat is older than
// the cutoff to offline. Returns how many were flipped.
func (s *MachineService) MarkStale(ctx context.Context, cutoff time.Time) (int, error) {
	// last_seen_at is stored in UTC (now()); the cutoff MUST match or the
	// lexicographic comparison lies — a local-zone cutoff (+08:00) compared
	// against UTC (Z) made every connected machine look stale, and the
	// sweep flapped offline→connected forever (live: an offline log every
	// 30s while the CLI kept heartbeating).
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE machine SET status='offline' WHERE status='connected' AND last_seen_at != '' AND last_seen_at < ?`,
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("mark stale machines: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarshalProbedCLIs encodes a probe report for storage.
func MarshalProbedCLIs(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// UpsertProbeRuntimes creates/updates one runtime row per probed CLI
// (CLI 分支 Phase 2): name = "<cli>@<machineName>", transport='agentwork',
// machine_id = the registered machine, args = the CLI's acp_spawn. Runs
// assigned to these runtimes are dispatched over the machine's /connect
// link. Keyed by runtime.name (UNIQUE) — a re-register refreshes the row.
// Disappeared CLIs keep their rows (their agents just don't get runs while
// the CLI is absent).
func (s *MachineService) UpsertProbeRuntimes(ctx context.Context, machineID, machineName string, clis []link.ProbeCLI) error {
	for _, c := range clis {
		argsJSON, _ := json.Marshal(c.ACPSpawn)
		name := c.Name + "@" + machineName
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT INTO runtime (id,name,machine_id,args,env,created_at)
			 VALUES (?,?,?,?,'{}',?)
			 ON CONFLICT(name) DO UPDATE SET
			   args=excluded.args, machine_id=excluded.machine_id`,
			newID(), name, machineID, string(argsJSON), now()); err != nil {
			return fmt.Errorf("upsert probe runtime %s: %w", name, err)
		}
	}
	return nil
}
