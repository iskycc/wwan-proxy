package vohive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
