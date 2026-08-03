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
	"strings"
	"testing"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/manager"
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
	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%v err=%v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	authBody := []byte(`{"username":"administrator","password":"StrongPassword!42"}`)
	resp, err = client.Post(ts.URL+"/api/auth/initialize", "application/json", bytes.NewReader(authBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize status=%v err=%v body=%s", resp.StatusCode, err, b)
	}
	_ = resp.Body.Close()

	cfg := config.Server{Enabled: false, Name: "wwan-test", Listen: "127.0.0.1:11880", Interface: "lo", Auth: config.Auth{Method: "none"}, UDP: config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto"}}
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
	if saved.ID == 0 {
		t.Fatal("configuration was not persisted")
	}

	resp, err = client.Get(ts.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), "wwan-test") || !strings.Contains(string(b), "heap_bytes") {
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
