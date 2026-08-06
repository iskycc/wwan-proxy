package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/proxyauth"
	"wwan-proxy/internal/store"
)

func TestWebUIAndConfigurationAPI(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(ctx, st, logger)
	defer mgr.Close()
	ui := New("127.0.0.1:0", st, mgr, logger)
	ts := httptest.NewServer(ui.http.Handler)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Contains(body, []byte("连接，一目了然")) {
		t.Fatal("WebUI index was not served")
	}
	if !bytes.Contains(body, []byte(`<body class="auth-pending">`)) || !bytes.Contains(body, []byte(`id="boot-screen"`)) {
		t.Fatal("WebUI does not hide login and dashboard content while authentication is pending")
	}
	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	resp, err = client.Get(ts.URL + "/api/interfaces")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated interfaces status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	authBody := []byte(`{"username":"administrator","password":"StrongPassword!42"}`)
	resp, err = client.Post(ts.URL+"/api/auth/initialize", "application/json", bytes.NewReader(authBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize status=%v err=%v body=%s", resp.StatusCode, err, b)
	}
	_ = resp.Body.Close()

	resp, err = client.Get(ts.URL + "/api/interfaces")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("interfaces status=%v err=%v", resp.StatusCode, err)
	}
	var interfaces []interfaceInfo
	if err := json.NewDecoder(resp.Body).Decode(&interfaces); err != nil || len(interfaces) == 0 {
		t.Fatalf("interfaces=%+v err=%v", interfaces, err)
	}
	_ = resp.Body.Close()

	cfg := config.Server{Enabled: false, Name: "wwan-test", Listen: "127.0.0.1:11880", HTTPProxy: config.HTTPProxy{Enabled: true, Listen: "127.0.0.1:18080"}, Interface: "lo", Bind: config.SOCKS5Bind{Enabled: true, Advertise: "127.0.0.1"}, Auth: config.Auth{Method: "username_password", Users: map[string]string{"proxy-user": "proxy-secret"}}, Access: config.AccessControl{AdmissionCIDRs: []string{"127.0.0.0/8"}, TargetDefault: "deny", TargetRules: []string{"allow *.example.com:443"}, MaxConnectionsPerIP: 4, MaxUDPAssociationsPerIP: 1}, DNS: config.DNS{IPv4Only: true, DoH: &config.DoH{URLs: []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"}, Timeout: config.Duration(time.Second)}}, UDP: config.UDP{Enabled: true, StrictEndpoint: true, BindIP: "127.0.0.1", Advertise: "auto", AdvertiseMap: map[string]string{"127.0.0.1": "127.0.0.2"}, RelayPorts: []int{14000, 14007, 14019}}}
	raw, _ := json.Marshal(cfg)
	resp, err = client.Post(ts.URL+"/api/servers", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var saved config.Server
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if saved.ID == 0 || !saved.HTTPProxy.Enabled || saved.HTTPProxy.Listen != "127.0.0.1:18080" || !saved.Bind.Enabled || saved.Bind.Advertise != "127.0.0.1" || saved.Access.TargetDefault != "deny" || saved.Access.MaxConnectionsPerIP != 4 || saved.Access.MaxUDPAssociationsPerIP != 1 || len(saved.UDP.RelayPorts) != 3 || saved.UDP.RelayPorts[1] != 14007 || !saved.UDP.StrictEndpoint || saved.UDP.AdvertiseMap["127.0.0.1"] != "127.0.0.2" || !saved.DNS.IPv4Only || len(saved.DNS.DoH.Endpoints()) != 2 {
		t.Fatalf("HTTP proxy configuration was not persisted: %+v", saved)
	}
	if saved.Auth.Users["proxy-user"] != "" || len(saved.Auth.PasswordUnchanged) != 1 || saved.Auth.PasswordUnchanged[0] != "proxy-user" {
		t.Fatalf("proxy password leaked in save response: %+v", saved.Auth.Users)
	}
	stored, err := st.GetServer(context.Background(), saved.ID)
	if err != nil || !proxyauth.IsHash(stored.Auth.Users["proxy-user"]) {
		t.Fatalf("proxy password was not hashed at rest: %+v err=%v", stored.Auth.Users, err)
	}

	resp, err = client.Post(ts.URL+"/api/servers/"+strconv.FormatInt(saved.ID, 10)+"/toggle", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("toggle status=%v err=%v body=%s", resp.StatusCode, err, b)
	}
	var toggled config.Server
	if err := json.NewDecoder(resp.Body).Decode(&toggled); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if toggled.Auth.Users["proxy-user"] != "" || len(toggled.Auth.PasswordUnchanged) != 1 {
		t.Fatalf("proxy password hash leaked in toggle response: %+v", toggled.Auth.Users)
	}
	resp, err = client.Post(ts.URL+"/api/servers/"+strconv.FormatInt(saved.ID, 10)+"/toggle", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("second toggle status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), "wwan-test") || !strings.Contains(string(b), "heap_bytes") || !strings.Contains(string(b), "heap_live_bytes") {
		t.Fatalf("unexpected overview %s", b)
	}

	resp, err = client.Get(ts.URL + "/api/settings")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("settings status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	settingsBody, _ := json.Marshal(map[string]any{
		"web_listen": "127.0.0.1:9191", "database_path": st.Path(),
		"log_retention_days": 60, "session_lifetime": "48h",
	})
	settingsRequest, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(settingsBody))
	settingsRequest.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(settingsRequest)
	if err != nil || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("save settings status=%v err=%v body=%s", resp.StatusCode, err, b)
	}
	_ = resp.Body.Close()

	adminBody := []byte(`{"username":"administrator","current_password":"StrongPassword!42","new_password":""}`)
	adminRequest, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/admin", bytes.NewReader(adminBody))
	adminRequest.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(adminRequest)
	if err != nil || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("update admin status=%v err=%v body=%s", resp.StatusCode, err, b)
	}
	_ = resp.Body.Close()

	resp, err = client.Get(ts.URL + "/api/sessions")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("sessions status=%v err=%v", resp.StatusCode, err)
	}
	var sessions []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil || len(sessions) != 1 || sessions[0]["current"] != true {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	_ = resp.Body.Close()

	resp, err = client.Post(ts.URL+"/api/auth/logout", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	resp, err = client.Get(ts.URL + "/api/auth/status")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("post-logout auth status=%v err=%v", resp.StatusCode, err)
	}
	var postLogoutStatus map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&postLogoutStatus); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if postLogoutStatus["initialized"] != true || postLogoutStatus["authenticated"] != false {
		t.Fatalf("post-logout auth state=%+v", postLogoutStatus)
	}

	resp, err = client.Post(ts.URL+"/api/auth/login", "application/json", bytes.NewReader(authBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status=%v err=%v body=%s", resp.StatusCode, err, b)
	}
	_ = resp.Body.Close()
	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("post-login status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	evilRequest, err := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	evilRequest.Header.Set("Origin", "https://attacker.example")
	resp, err = client.Do(evilRequest)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
}

func TestVohiveEventsAPI(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "vohive-web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(ctx, st, logger)
	defer mgr.Close()
	ui := New("127.0.0.1:0", st, mgr, logger)
	ts := httptest.NewServer(ui.http.Handler)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Initialize admin.
	authBody := []byte(`{"username":"administrator","password":"StrongPassword!42"}`)
	resp, err := client.Post(ts.URL+"/api/auth/initialize", "application/json", bytes.NewReader(authBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("initialize status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// Unauthenticated request should fail.
	unauth := &http.Client{}
	resp, err = unauth.Get(ts.URL + "/api/vohive/events")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()

	// Seed an event.
	serverID := int64(42)
	_, err = st.SaveVohiveEvent(context.Background(), store.VohiveEvent{
		Type:      store.VohiveEventDegraded,
		DeviceID:  "Y2",
		ServerID:  &serverID,
		Message:   "link down",
		Details:   map[string]any{"signal": -90},
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// GET /api/vohive/events returns the event.
	resp, err = client.Get(ts.URL + "/api/vohive/events")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("events status=%v err=%v", resp.StatusCode, err)
	}
	var events []store.VohiveEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(events) != 1 || events[0].DeviceID != "Y2" || events[0].Type != store.VohiveEventDegraded {
		t.Fatalf("unexpected events: %+v", events)
	}

	// Filter by device.
	resp, err = client.Get(ts.URL + "/api/vohive/events?device=Y2")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered events status=%v err=%v", resp.StatusCode, err)
	}
	events = nil
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(events) != 1 {
		t.Fatalf("expected 1 event for device filter, got %d", len(events))
	}

	// Filter by type.
	resp, err = client.Get(ts.URL + "/api/vohive/events?type=degraded")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered events status=%v err=%v", resp.StatusCode, err)
	}
	events = nil
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(events) != 1 {
		t.Fatalf("expected 1 event for type filter, got %d", len(events))
	}

	// Overview includes vohive_events.
	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("overview status=%v err=%v", resp.StatusCode, err)
	}
	var overview map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, ok := overview["vohive_events"]; !ok {
		t.Fatal("overview missing vohive_events")
	}
}

func newTestServer(t *testing.T) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(ctx, st, logger)
	t.Cleanup(mgr.Close)
	ui := New("127.0.0.1:0", st, mgr, logger)
	ts := httptest.NewServer(ui.http.Handler)
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Initialize admin so authenticated endpoints are reachable.
	authBody := []byte(`{"username":"administrator","password":"StrongPassword!42"}`)
	resp, err := client.Post(ts.URL+"/api/auth/initialize", "application/json", bytes.NewReader(authBody))
	if err != nil {
		t.Fatalf("initialize admin: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("initialize admin status=%d", resp.StatusCode)
	}
	return ts, st, client
}

func TestStatsHandlers(t *testing.T) {
	ts, st, client := newTestServer(t)

	ctx := context.Background()
	cfg := config.Server{Enabled: true, Name: "stats-test", Listen: "127.0.0.1:11090", Interface: "lo", Auth: config.Auth{Method: "none"}}
	if err := st.SaveServer(ctx, &cfg); err != nil {
		t.Fatalf("save server: %v", err)
	}
	if cfg.ID == 0 {
		t.Fatal("server id not assigned")
	}

	bucket := time.Now().UTC().Truncate(time.Minute)
	if err := st.SaveServerStats(ctx, []store.ServerStats{{
		ServerID:           cfg.ID,
		Bucket:             bucket,
		TCPUploadBytes:     100,
		TCPDownloadBytes:   200,
		ActiveConnections:  3,
		HeartbeatLatencyMs: 20,
		HeartbeatHealthy:   true,
		InstanceStartedAt:  bucket,
	}}); err != nil {
		t.Fatalf("save server stats: %v", err)
	}

	resp, err := client.Get(ts.URL + "/api/stats/servers")
	if err != nil {
		t.Fatalf("stats servers request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats servers status=%d body=%s", resp.StatusCode, body)
	}
	var servers []map[string]any
	if err := json.Unmarshal(body, &servers); err != nil {
		t.Fatalf("decode stats servers: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}

	resp, err = client.Get(ts.URL + "/api/stats?server_id=" + strconv.FormatInt(cfg.ID, 10))
	if err != nil {
		t.Fatalf("stats request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", resp.StatusCode, body)
	}
	var stats []map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stats row, got %d", len(stats))
	}
	if stats[0]["upload_bytes"] == nil || stats[0]["download_bytes"] == nil {
		t.Fatalf("stats response missing upload/download bytes: %+v", stats[0])
	}
	if stats[0]["bucket"] == "" {
		t.Fatalf("stats response missing bucket: %+v", stats[0])
	}

	resp, err = client.Get(ts.URL + "/api/stats/summary?server_id=" + strconv.FormatInt(cfg.ID, 10))
	if err != nil {
		t.Fatalf("stats summary request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats summary status=%d body=%s", resp.StatusCode, body)
	}
	var summary map[string]any
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatalf("decode stats summary: %v", err)
	}
	if summary["total_buckets"] != float64(1) {
		t.Fatalf("expected 1 total bucket, got %v", summary["total_buckets"])
	}
	if summary["success_rate"] == nil {
		t.Fatal("summary response missing success_rate")
	}

	// Validation error: invalid step.
	resp, err = client.Get(ts.URL + "/api/stats?server_id=" + strconv.FormatInt(cfg.ID, 10) + "&step=week")
	if err != nil {
		t.Fatalf("invalid step request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid step status=%d body=%s", resp.StatusCode, body)
	}

	// Validation error: from after to.
	resp, err = client.Get(ts.URL + "/api/stats?server_id=" + strconv.FormatInt(cfg.ID, 10) + "&from=2024-01-02T00:00:00Z&to=2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("from after to request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("from after to status=%d body=%s", resp.StatusCode, body)
	}

	// Validation error: invalid server_id.
	resp, err = client.Get(ts.URL + "/api/stats?server_id=not-a-number")
	if err != nil {
		t.Fatalf("invalid server_id request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid server_id status=%d body=%s", resp.StatusCode, body)
	}
}

func TestWebListenNetworkRespectsLiteralAddressFamily(t *testing.T) {
	tests := map[string]string{
		"0.0.0.0:9090":      "tcp4",
		"127.0.0.1:9090":    "tcp4",
		"[::]:9090":         "tcp6",
		"[::1]:9090":        "tcp6",
		"[fe80::1%lo]:9090": "tcp6",
		"localhost:9090":    "tcp",
		":9090":             "tcp",
		"not-an-address":    "tcp",
	}
	for address, want := range tests {
		if got := webListenNetwork(address); got != want {
			t.Errorf("webListenNetwork(%q)=%q, want %q", address, got, want)
		}
	}
}
