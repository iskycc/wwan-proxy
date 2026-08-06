package vohive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRestartDevice(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("expected PATCH, got %s", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer token123" {
			t.Fatalf("unexpected auth: %s", auth)
		}
		calls = append(calls, r.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		enabled, _ := body["enabled"].(bool)
		w.Header().Set("Content-Type", "application/json")
		if enabled {
			_ = json.NewEncoder(w).Encode(NetworkStatus{
				Device: "Y2", NetworkConnected: true, PublicIP: "", Status: "ok",
			})
		} else {
			_ = json.NewEncoder(w).Encode(NetworkStatus{
				Device: "Y2", NetworkConnected: false, Status: "ok",
			})
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token123", 10*time.Second)
	status, err := c.RestartDevice(context.Background(), "Y2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.NetworkConnected {
		t.Fatal("expected network_connected true after restart")
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
}

func TestGetNetworkStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(NetworkStatus{
			Device: "Y2", NetworkConnected: true, PublicIP: "203.0.113.5", Status: "ok",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token123", 10*time.Second)
	status, err := c.GetNetworkStatus(context.Background(), "Y2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.PublicIP != "203.0.113.5" {
		t.Fatalf("unexpected public ip: %s", status.PublicIP)
	}
}
