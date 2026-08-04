package webui

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/store"
)

func TestRestartRequiredUsesPersistedStartupBaseline(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(context.Background(), st, logger)
	ui := New("0.0.0.0:19090", st, mgr, logger) // Simulates a -web override.

	settings, err := st.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.DatabasePath = st.Path()
	response := ui.settingsResponse(settings)
	if response.RestartRequired {
		t.Fatalf("CLI listen override produced a false restart warning: %+v", response)
	}
	if response.CurrentWebListen != "0.0.0.0:19090" {
		t.Fatalf("current listener=%q", response.CurrentWebListen)
	}

	settings.WebListen = "127.0.0.1:19091"
	if !ui.settingsResponse(settings).RestartRequired {
		t.Fatal("changed persisted WebUI listener did not require restart")
	}
	settings.WebListen = ui.startupWebListen
	settings.DatabasePath = filepath.Join(t.TempDir(), "next.db")
	if !ui.settingsResponse(settings).RestartRequired {
		t.Fatal("changed database path did not require restart")
	}
}
