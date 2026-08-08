package vohive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGetHealthParsesDevices(t *testing.T) {
	resp := HealthResponse{
		Status: "healthy",
		Devices: map[string]DeviceHealth{
			"Y1": {Healthy: true, ModemOK: true, IfaceUp: true, NetworkConnected: true, Signal: -57},
			"Y2": {Healthy: true, ModemOK: true, IfaceUp: false, NetworkConnected: false, Signal: -59},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/api/auth/login" {
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case http.MethodGet:
			if r.URL.Path != "/api/health" {
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			auth := r.Header.Get("Authorization")
			if auth != "Bearer token123" {
				t.Fatalf("unexpected auth: %s", auth)
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	got, err := client.GetHealth(context.Background())
	if err != nil {
		t.Fatalf("GetHealth error: %v", err)
	}
	if got.Status != "healthy" {
		t.Fatalf("status = %q, want healthy", got.Status)
	}
	if len(got.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(got.Devices))
	}
	if !got.Devices["Y1"].Healthy {
		t.Fatal("Y1 should be healthy")
	}
}

func TestGetHealthRetriesOnceOn401(t *testing.T) {
	loginCount := 0
	healthCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			loginCount++
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/health":
			healthCount++
			auth := r.Header.Get("Authorization")
			if auth != "Bearer token123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(HealthResponse{Status: "healthy", Devices: map[string]DeviceHealth{}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	// Seed an invalid token so the first health request is rejected.
	client.token = "bad"
	client.expiresAt = time.Now().Add(time.Hour)

	got, err := client.GetHealth(context.Background())
	if err != nil {
		t.Fatalf("GetHealth error: %v", err)
	}
	if got.Status != "healthy" {
		t.Fatalf("status = %q, want healthy", got.Status)
	}
	if loginCount != 1 {
		t.Fatalf("loginCount = %d, want 1", loginCount)
	}
	if healthCount != 2 {
		t.Fatalf("healthCount = %d, want 2", healthCount)
	}
}

func TestGetHealthPersistent401ReturnsError(t *testing.T) {
	loginCount := 0
	healthCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			loginCount++
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/health":
			healthCount++
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	_, err := client.GetHealth(context.Background())
	if err == nil {
		t.Fatal("expected error for persistent 401")
	}
	if healthCount > 2 {
		t.Fatalf("too many health requests: %d", healthCount)
	}
	if loginCount > 2 {
		t.Fatalf("too many login requests: %d", loginCount)
	}
}

func TestGetHealthNon2XXReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/health":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	_, err := client.GetHealth(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestGetHealthNon2XXReturnsParsedHealthPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/health":
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(HealthResponse{
				Status: "unhealthy",
				Devices: map[string]DeviceHealth{
					"Y1": {Healthy: true, ModemOK: true, IfaceUp: true, NetworkConnected: true, Signal: -53},
					"Y4": {Healthy: false, ModemOK: false},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	health, err := client.GetHealth(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
	if health == nil || health.Status != "unhealthy" || len(health.Devices) != 2 {
		t.Fatalf("health = %+v, want parsed unhealthy response", health)
	}
	if health.Devices["Y4"].Healthy {
		t.Fatal("Y4 should be unhealthy")
	}
}

func TestDeviceNetworkPathEscapes(t *testing.T) {
	id := "device/with space"
	got := deviceNetworkPath(id)
	want := "/api/devices/" + url.PathEscape(id) + "/network"
	if got != want {
		t.Fatalf("deviceNetworkPath(%q) = %q, want %q", id, got, want)
	}
}

func TestRestartDeviceBestEffortReEnable(t *testing.T) {
	var disableCount, enableCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/devices/Y1/network":
			var payload map[string]bool
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			if payload["enabled"] {
				enableCount++
				if enableCount == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(NetworkStatus{Device: "Y1", NetworkConnected: true})
			} else {
				disableCount++
				_ = json.NewEncoder(w).Encode(NetworkStatus{Device: "Y1", NetworkConnected: false})
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	_, err := client.RestartDevice(context.Background(), "Y1")
	if err == nil {
		t.Fatal("expected error when enable fails")
	}
	if disableCount != 1 {
		t.Fatalf("disableCount = %d, want 1", disableCount)
	}
	if enableCount != 2 {
		t.Fatalf("enableCount = %d, want 2", enableCount)
	}
}
