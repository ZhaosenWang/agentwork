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
// marks it connected with the fresh probe report. Machine names are UNIQUE:
// runtime rows are keyed by "<cli>@<name>" (UpsertProbeRuntimes), so a
// second machine registering the same name would silently steal the first
// machine's runtime rows and reroute its runs — reject with a pointer to
// --name instead. Re-registering under its own name is fine.
func (s *MachineService) Register(ctx context.Context, m Machine, probedCLIsJSON string) error {
	if m.ID == "" {
		return NewFieldRequiredError("machine_id")
	}
	if m.Name == "" {
		return NewFieldRequiredError("name")
	}
	var holder string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM machine WHERE name=? AND id != ?`, m.Name, m.ID).Scan(&holder); err == nil {
		return NewValidationError(fmt.Sprintf(
			"machine name %q is already registered by machine %s — pass --name to choose a unique one", m.Name, holder))
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

// UpdateProbe stores a fresh probe report AND reconciles the machine's
// runtime rows against it (a CLI that vanished from the probe is marked
// absent — the claim gate then stops dispatching to it).
func (s *MachineService) UpdateProbe(ctx context.Context, machineID, probedCLIsJSON string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE machine SET probed_clis=?, last_seen_at=? WHERE id=?`,
		probedCLIsJSON, now(), machineID); err != nil {
		return fmt.Errorf("machine probe update: %w", err)
	}
	var name string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT name FROM machine WHERE id=?`, machineID).Scan(&name); err != nil {
		return err
	}
	var clis []link.ProbeCLI
	if err := json.Unmarshal([]byte(probedCLIsJSON), &clis); err != nil {
		return nil
	}
	return s.ReconcileProbeRuntimes(ctx, machineID, name, clis)
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

// MarkOffline marks one machine offline immediately (graceful CLI shutdown via
// machine.offline notification). Idempotent: only a connected machine flips,
// so a second notification is a no-op. Does not touch last_seen_at — the
// stale sweep still owns that field. A notification from a dead link that
// races a reconnect can clobber the new connected state, but the next
// heartbeat (≤5s) restores it — a tolerable flap vs the 90s it replaces.
func (s *MachineService) MarkOffline(ctx context.Context, machineID string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`UPDATE machine SET status='offline' WHERE id=? AND status='connected'`, machineID)
	if err != nil {
		return fmt.Errorf("mark machine offline: %w", err)
	}
	return nil
}

// MarshalProbedCLIs encodes a probe report for storage.
func MarshalProbedCLIs(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ReconcileProbeRuntimes syncs the machine's runtime rows with a probe
// report (CLI 分支 Phase 2 + probe reconciliation): one row per probed
// CLI — name = "<cli>@<machineName>", keyed by runtime.name (UNIQUE) —
// upserted and marked active. Rows of THIS machine that are NOT in the
// report are marked ABSENT (the CLI was uninstalled): the row survives
// (agents reference it) but the claim gate rejects it and the web shows
// the absence. Runs assigned to active runtimes dispatch over the
// machine's /connect link.
func (s *MachineService) ReconcileProbeRuntimes(ctx context.Context, machineID, machineName string, clis []link.ProbeCLI) error {
	present := map[string]bool{}
	for _, c := range clis {
		argsJSON, _ := json.Marshal(c.ACPSpawn)
		name := c.Name + "@" + machineName
		present[name] = true
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at)
			 VALUES (?,?,?,?,'{}','active',?)
			 ON CONFLICT(name) DO UPDATE SET
			   args=excluded.args, machine_id=excluded.machine_id, status='active'`,
			newID(), name, machineID, string(argsJSON), now()); err != nil {
			return fmt.Errorf("upsert probe runtime %s: %w", name, err)
		}
	}
	if len(present) == 0 {
		if _, err := s.st.DB().ExecContext(ctx,
			`UPDATE runtime SET status='absent' WHERE machine_id=? AND status='active'`, machineID); err != nil {
			return fmt.Errorf("mark runtimes absent: %w", err)
		}
		return nil
	}
	names := make([]string, 0, len(present))
	for n := range present {
		names = append(names, n)
	}
	ph, phArgs := inPlaceholders(names)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE runtime SET status='absent' WHERE machine_id=? AND status='active' AND name NOT IN (`+ph+`)`,
		append([]any{machineID}, phArgs...)...); err != nil {
		return fmt.Errorf("mark runtimes absent: %w", err)
	}
	return nil
}
