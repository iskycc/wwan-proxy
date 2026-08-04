package store

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/proxyauth"
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

func TestDatabaseFilePermissionsAreRestricted(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("database mode=%#o, want 0600", got)
	}
}

func TestDatabaseCloneIsRestrictedWithPermissiveUmask(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	target := filepath.Join(t.TempDir(), "clone.db")
	previousUmask := syscall.Umask(0)
	defer syscall.Umask(previousUmask)
	if err := s.cloneTo(target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("cloned database mode=%#o, want 0600", got)
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

func TestAccessAndBindConfigurationPersistence(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cfg := testServer("secured", "127.0.0.1:11084")
	cfg.Bind = config.SOCKS5Bind{Enabled: true}
	cfg.Access = config.AccessControl{
		AdmissionCIDRs: []string{"10.0.0.0/8"}, TargetDefault: "deny",
		TargetRules:         []string{"allow *.example.com:443"},
		MaxConnectionsPerIP: 8, MaxUDPAssociationsPerIP: 1,
	}
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Bind.Enabled || got.Access.TargetDefault != "deny" || len(got.Access.AdmissionCIDRs) != 1 || len(got.Access.TargetRules) != 1 || got.Access.MaxConnectionsPerIP != 8 || got.Access.MaxUDPAssociationsPerIP != 1 {
		t.Fatalf("access/BIND configuration was not preserved: %+v", got)
	}
}

func TestProxyCredentialsAreHashedAndLegacyRowsMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testServer("hashed", "127.0.0.1:11085")
	cfg.Auth = config.Auth{Method: "username_password", Users: map[string]string{"alice": "plain-secret"}}
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !proxyauth.IsHash(cfg.Auth.Users["alice"]) || strings.Contains(cfg.Auth.Users["alice"], "plain-secret") {
		t.Fatal("SaveServer did not replace plaintext credentials")
	}
	if _, err := s.db.Exec(`PRAGMA secure_delete=OFF`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	legacy := `{"name":"legacy-auth","listen":"127.0.0.1:11086","interface":"lo","auth":{"method":"username_password","users":{"bob":"legacy-secret"}}}`
	if _, err := s.db.Exec(`INSERT INTO server_configs(name,enabled,config_json,created_at,updated_at) VALUES(?,?,?,?,?)`, "legacy-auth", 0, legacy, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	servers, err := s.ListServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		for _, password := range server.Auth.Users {
			if !proxyauth.IsHash(password) {
				t.Fatalf("server %s retained a plaintext password", server.Name)
			}
		}
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		content, readErr := os.ReadFile(candidate)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(content), "legacy-secret") {
			t.Fatalf("legacy plaintext remained recoverable in %s", candidate)
		}
	}
}

func TestCredentialCleanupMigrationResumesAfterCommittedHashUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resume-cleanup.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA secure_delete=OFF`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	const leaked = "crash-window-plaintext"
	raw := `{"name":"resume","listen":"127.0.0.1:11091","interface":"lo","auth":{"method":"username_password","users":{"alice":"` + leaked + `"}}}`
	result, err := s.db.Exec(`INSERT INTO server_configs(name,enabled,config_json,created_at,updated_at) VALUES(?,?,?,?,?)`, "resume", 0, raw, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	hash, err := proxyauth.Hash(leaked)
	if err != nil {
		t.Fatal(err)
	}
	// Emulate a crash after the logical credential update committed but before
	// checkpoint/VACUUM completed. The persistent pending bit must make the next
	// Open repeat physical cleanup even though no plaintext logical value remains.
	hashedRaw := `{"name":"resume","listen":"127.0.0.1:11091","interface":"lo","auth":{"method":"username_password","users":{"alice":"` + hash + `"}}}`
	if _, err := s.db.Exec(`UPDATE security_migrations SET cleanup_pending=1 WHERE name='proxy_credentials_v1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE server_configs SET config_json=? WHERE id=?`, hashedRaw, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var cleanupPending bool
	if err := s.db.QueryRow(`SELECT cleanup_pending FROM security_migrations WHERE name='proxy_credentials_v1'`).Scan(&cleanupPending); err != nil {
		t.Fatal(err)
	}
	if cleanupPending {
		t.Fatal("resumed credential cleanup did not clear its persistent pending marker")
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		content, readErr := os.ReadFile(candidate)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(content), leaked) {
			t.Fatalf("resumed cleanup left plaintext recoverable in %s", candidate)
		}
	}
}

func TestProxyCredentialPlaceholderSurvivesAuthenticationToggle(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cfg := testServer("auth-toggle", "127.0.0.1:11087")
	cfg.Auth = config.Auth{Method: "username_password", Users: map[string]string{"alice": "plain-secret"}}
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	original := cfg.Auth.Users["alice"]

	cfg.Auth.Method = "none"
	cfg.Auth.Users["alice"] = proxyauth.Redacted
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetServer(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Auth.Users["alice"] != original || !proxyauth.Verify(stored.Auth.Users["alice"], "plain-secret") {
		t.Fatalf("authentication toggle replaced the stored credential: %q", stored.Auth.Users["alice"])
	}
}

func TestEightAsterisksCanBeStoredAsARealPasswordWithStructuredMarker(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cfg := testServer("asterisk-password", "127.0.0.1:11088")
	cfg.Auth = config.Auth{
		Method: "username_password", Users: map[string]string{"alice": proxyauth.Redacted},
		PasswordUnchanged: []string{},
	}
	if err := s.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !proxyauth.IsHash(cfg.Auth.Users["alice"]) || !proxyauth.Verify(cfg.Auth.Users["alice"], proxyauth.Redacted) {
		t.Fatal("eight-asterisk password was confused with an unchanged marker")
	}
}

func TestAPIInputHashesHashShapedLiteralPassword(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	literal, err := proxyauth.Hash("different-secret")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testServer("hash-shaped-password", "127.0.0.1:11089")
	cfg.Auth = config.Auth{Method: "username_password", Users: map[string]string{"alice": literal}, PasswordUnchanged: []string{}}
	if err := s.SaveServerInput(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Users["alice"] == literal || !proxyauth.Verify(cfg.Auth.Users["alice"], literal) {
		t.Fatal("hash-shaped API password was mistaken for an internal stored hash")
	}
}

func TestAPIInputPreservesStructuredUnchangedPassword(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	cfg := testServer("structured-password", "127.0.0.1:11090")
	cfg.Auth = config.Auth{Method: "username_password", Users: map[string]string{"alice": "original-secret"}}
	if err := s.SaveServerInput(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	original := cfg.Auth.Users["alice"]
	cfg.Auth.Users["alice"] = ""
	cfg.Auth.PasswordUnchanged = []string{"alice"}
	if err := s.SaveServerInput(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Users["alice"] != original || !proxyauth.Verify(cfg.Auth.Users["alice"], "original-secret") {
		t.Fatal("structured unchanged password was re-hashed or replaced")
	}
}

func TestLegacyServerJSONReceivesAccessAndBindDefaults(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(`INSERT INTO server_configs(name,enabled,config_json,created_at,updated_at) VALUES(?,?,?,?,?)`,
		"legacy", 0, `{"name":"legacy","listen":"127.0.0.1:1080","interface":"lo","udp":{"bind_ip":"0:0:0:0:0:0:0:0","advertise":"auto","advertise_map":{"0:0:0:0:0:0:0:1":"2001:0db8:0:0:0:0:0:1"}}}`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	got, err := s.GetServer(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bind.Enabled || got.Access.TargetDefault != "allow" {
		t.Fatalf("legacy defaults are unsafe or incompatible: %+v", got)
	}
	if got.UDP.BindIP != "::" || got.UDP.AdvertiseMap["::1"] != "2001:db8::1" || len(got.UDP.AdvertiseMap) != 1 {
		t.Fatalf("legacy UDP advertise map was not normalized: %+v", got.UDP)
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
	if err != nil || len(all) != 1 {
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

func TestPersistentHandlerRuntimeLevelFiltersConsoleAndSQLite(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	var console bytes.Buffer
	handler, flush := NewPersistentHandler(slog.NewTextHandler(&console, &slog.HandlerOptions{Level: slog.LevelDebug}), s)
	logger := slog.New(handler)

	if got := handler.Level(); got != "WARN" {
		t.Fatalf("default level=%q", got)
	}
	logger.Debug("hidden debug")
	logger.Info("hidden info")
	logger.Warn("visible warning")
	logger.Error("visible error")
	if err := handler.SetLevel("error"); err != nil {
		t.Fatal(err)
	}
	logger.Warn("hidden after update")
	logger.Error("visible after update")
	if err := handler.SetLevel("verbose"); err == nil {
		t.Fatal("invalid runtime log level accepted")
	}
	flush()

	output := console.String()
	for _, hidden := range []string{"hidden debug", "hidden info", "hidden after update"} {
		if strings.Contains(output, hidden) {
			t.Fatalf("console contains filtered record %q: %s", hidden, output)
		}
	}
	for _, visible := range []string{"visible warning", "visible error", "visible after update"} {
		if !strings.Contains(output, visible) {
			t.Fatalf("console is missing %q: %s", visible, output)
		}
	}
	logs, err := s.ListLogs(context.Background(), 10, "ALL", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("SQLite retained filtered records: %+v", logs)
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
