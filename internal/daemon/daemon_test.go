package daemon

import (
	"context"
	"testing"

	"github.com/eushing/agentwork/internal/store"
)

// TestAdvertisedBaseURL: the advertised base URL is configurable via
// app_settings platform.advertise_url (remote agents need the daemon's
// public address — 127.0.0.1 is unreachable for them); unset falls back to
// http://127.0.0.1:<listen port>.
func TestAdvertisedBaseURL(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{st: st, addr: ":7373"}
	ctx := context.Background()

	got, err := d.advertisedBaseURL(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:7373" {
		t.Fatalf("default must be http://127.0.0.1:7373, got %q", got)
	}

	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES ('platform.advertise_url','https://agentwork.example.com/',?)`, nowStr()); err != nil {
		t.Fatal(err)
	}
	got, err = d.advertisedBaseURL(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://agentwork.example.com" {
		t.Fatalf("the configured URL must win (trailing slash trimmed), got %q", got)
	}
}
