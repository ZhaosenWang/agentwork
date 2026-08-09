package service

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/eushing/agentwork/internal/store"
)

// SettingsService is a tiny key-value store for daemon configuration (the
// IM connection credentials and receive target, M1). Persisted in SQLite so
// the daemon auto-reconnects on startup without environment configuration.
type SettingsService struct {
	st *store.Store
}

func NewSettingsService(st *store.Store) *SettingsService {
	return &SettingsService{st: st}
}

// Get returns the JSON value for key ("" when unset).
func (s *SettingsService) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.st.DB().QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("settings get %s: %w", key, err)
	}
	return v, nil
}

// Set stores the JSON value under key (upsert).
func (s *SettingsService) Set(ctx context.Context, key, value string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now()); err != nil {
		return fmt.Errorf("settings set %s: %w", key, err)
	}
	return nil
}

// Delete removes a key (missing key is a no-op).
func (s *SettingsService) Delete(ctx context.Context, key string) error {
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM app_settings WHERE key=?`, key); err != nil {
		return fmt.Errorf("settings delete %s: %w", key, err)
	}
	return nil
}
