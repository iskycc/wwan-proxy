package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/store"
)

type recordingLogLevelSetter struct{ level string }

func (s *recordingLogLevelSetter) SetLevel(level string) error {
	s.level = level
	return nil
}

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

func TestSaveSettingsAppliesLogLevelImmediately(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(context.Background(), st, logger)
	setter := new(recordingLogLevelSetter)
	ui := New("127.0.0.1:19090", st, mgr, logger, setter)

	payload := map[string]any{
		"web_listen": "127.0.0.1:19090", "database_path": st.Path(),
		"log_level": "ERROR", "log_retention_days": 30, "session_lifetime": "24h0m0s",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	ui.saveSettings(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if setter.level != "ERROR" {
		t.Fatalf("runtime log level=%q", setter.level)
	}
	persisted, err := st.SystemSettings(context.Background())
	if err != nil || persisted.LogLevel != "ERROR" {
		t.Fatalf("persisted settings=%+v err=%v", persisted, err)
	}
}
