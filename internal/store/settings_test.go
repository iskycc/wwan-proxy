package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestSystemSettingsAndDatabaseRelocation(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original.db")
	target := filepath.Join(dir, "relocated", "proxy.db")
	s, err := Open(original)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testServer("relocation", "127.0.0.1:14080")
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	settings := config.SystemSettings{WebListen: "127.0.0.1:9191", DatabasePath: target, LogLevel: "ERROR", LogRetentionDays: 45, SessionLifetime: config.Duration(48 * time.Hour)}
	if err := s.SaveSystemSettings(context.Background(), &settings); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(original)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Path() != target || reopened.BootstrapPath() != original {
		t.Fatalf("path=%q bootstrap=%q", reopened.Path(), reopened.BootstrapPath())
	}
	configs, err := reopened.ListServers(context.Background())
	if err != nil || len(configs) != 1 || configs[0].Name != "relocation" {
		t.Fatalf("configs=%+v err=%v", configs, err)
	}
	got, err := reopened.SystemSettings(context.Background())
	if err != nil || got.WebListen != settings.WebListen || got.LogLevel != "ERROR" || got.LogRetentionDays != 45 {
		t.Fatalf("settings=%+v err=%v", got, err)
	}
	secondTarget := filepath.Join(dir, "second", "proxy.db")
	got.DatabasePath = secondTarget
	if err := reopened.SaveSystemSettings(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(original)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Path() != secondTarget {
		t.Fatalf("second relocation path=%q", reopened.Path())
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Path() != secondTarget {
		t.Fatalf("flattened relocation path=%q", reopened.Path())
	}
}

func TestSystemSettingsValidation(t *testing.T) {
	settings := config.SystemSettings{WebListen: "bad", LogRetentionDays: 30, SessionLifetime: config.Duration(time.Hour)}
	if err := settings.Validate(); err == nil {
		t.Fatal("invalid web listen accepted")
	}
	settings.WebListen = "127.0.0.1:9090"
	settings.DatabasePath = "relative.db"
	if err := settings.Validate(); err == nil {
		t.Fatal("relative database path accepted")
	}
	settings.DatabasePath = ""
	settings.LogLevel = "trace"
	if err := settings.Validate(); err == nil {
		t.Fatal("invalid log level accepted")
	}
}

func TestSystemSettingsWebDefaultOnlyAppliesWithoutPersistedValue(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	standard, err := s.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if standard.WebListen != "127.0.0.1:9090" {
		t.Fatalf("standard default=%q", standard.WebListen)
	}
	alpine, err := s.SystemSettingsWithWebDefault(context.Background(), "0.0.0.0:9090")
	if err != nil {
		t.Fatal(err)
	}
	if alpine.WebListen != "0.0.0.0:9090" {
		t.Fatalf("Alpine default=%q", alpine.WebListen)
	}
	var rows int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM system_settings`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("reading a platform default unexpectedly persisted %d settings rows", rows)
	}
	if err := s.SetWebListenDefault("0.0.0.0:9090"); err != nil {
		t.Fatal(err)
	}
	processDefault, err := s.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processDefault.WebListen != "0.0.0.0:9090" {
		t.Fatalf("process-wide default=%q", processDefault.WebListen)
	}

	alpine.WebListen = "127.0.0.1:9191"
	if err := s.SaveSystemSettings(context.Background(), &alpine); err != nil {
		t.Fatal(err)
	}
	persisted, err := s.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WebListen != "127.0.0.1:9191" {
		t.Fatalf("platform default overrode persisted listener: %q", persisted.WebListen)
	}
}

func TestSystemSettingsRejectsInvalidWebDefault(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SystemSettingsWithWebDefault(context.Background(), "missing-port"); err == nil {
		t.Fatal("invalid WebUI default was accepted")
	}
}

func TestOpenWithWebDefaultPreservesPersistedListenerAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	s, err := OpenWithWebDefault(path, "0.0.0.0:9090")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.WebListen != "0.0.0.0:9090" {
		t.Fatalf("first-run listener=%q", settings.WebListen)
	}
	settings.WebListen = "127.0.0.1:9191"
	if err := s.SaveSystemSettings(context.Background(), &settings); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithWebDefault(path, "0.0.0.0:9090")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.SystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WebListen != "127.0.0.1:9191" {
		t.Fatalf("platform default overrode reopened persisted listener: %q", persisted.WebListen)
	}
}
