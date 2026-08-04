package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestServerPersistenceAndConflicts(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cfg := testServer("one", "127.0.0.1:11080")
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ID == 0 {
		t.Fatal("ID was not assigned")
	}
	list, err := s.ListServers(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != cfg.ID {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	duplicate := testServer("two", cfg.Listen)
	if err := s.SaveServer(context.Background(), &duplicate); err == nil {
		t.Fatal("expected listen conflict")
	}
	cfg.Enabled = false
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(context.Background(), cfg.ID)
	if err != nil || got.Enabled {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestHTTPProxyListenConflicts(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	first := testServer("one", "127.0.0.1:11080")
	first.HTTPProxy = config.HTTPProxy{Enabled: true, Listen: "127.0.0.1:18080"}
	if err := s.SaveServer(context.Background(), &first); err != nil {
		t.Fatal(err)
	}

	tests := []config.Server{
		testServer("socks-vs-http", first.HTTPProxy.Listen),
		testServer("http-vs-socks", "127.0.0.1:11081"),
		testServer("http-vs-http", "127.0.0.1:11082"),
	}
	tests[1].HTTPProxy = config.HTTPProxy{Enabled: true, Listen: first.Listen}
	tests[2].HTTPProxy = config.HTTPProxy{Enabled: true, Listen: first.HTTPProxy.Listen}
	for _, cfg := range tests {
		if err := s.SaveServer(context.Background(), &cfg); err == nil {
			t.Fatalf("expected listener conflict for %+v", cfg)
		}
	}

	disabled := testServer("disabled-http", "127.0.0.1:11083")
	disabled.HTTPProxy = config.HTTPProxy{Enabled: false, Listen: first.HTTPProxy.Listen}
	if err := s.SaveServer(context.Background(), &disabled); err != nil {
		t.Fatalf("disabled HTTP listener should not reserve a port: %v", err)
	}
}

func TestLogsAndHeartbeatPersistence(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cfg := testServer("one", "127.0.0.1:11080")
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	console := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	handler, flush := NewPersistentHandler(console, s)
	logger := slog.New(handler).With("component", "test", "server", "one")
	logger.Debug("debug detail", "request_id", "abc")
	logger.Error("dial failed", "error", context.DeadlineExceeded)
	flush()
	logs, err := s.ListLogs(context.Background(), 10, "ERROR", "dial")
	if err != nil || len(logs) != 1 || logs[0].ServerName != "one" {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	all, err := s.ListLogs(context.Background(), 10, "ALL", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	h := Heartbeat{ServerID: cfg.ID, CheckedAt: time.Now(), Healthy: false, LatencyMS: 12000, Error: "network unreachable"}
	if err := s.SaveHeartbeat(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	heartbeats, err := s.ListHeartbeats(context.Background())
	if err != nil || heartbeats[cfg.ID].Error != h.Error {
		t.Fatalf("heartbeats=%+v err=%v", heartbeats, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testServer(name, listen string) config.Server {
	return config.Server{Enabled: true, Name: name, Listen: listen, Interface: "lo", Auth: config.Auth{Method: "none"}, UDP: config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto"}}
}
