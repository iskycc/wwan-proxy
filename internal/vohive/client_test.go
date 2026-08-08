package vohive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestartDeviceLogsInAndRestarts(t *testing.T) {
	var calls []string
	var loginCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/api/auth/login" {
				t.Fatalf("expected login path, got %s", r.URL.Path)
			}
			loginCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "user" || body["password"] != "pass" {
				t.Fatalf("unexpected credentials: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
		case http.MethodPatch:
			auth := r.Header.Get("Authorization")
			if auth != "Bearer token123" {
				t.Fatalf("unexpected auth: %s", auth)
			}
			calls = append(calls, r.URL.Path)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			enabled, _ := body["enabled"].(bool)
			if enabled {
				_ = json.NewEncoder(w).Encode(NetworkStatus{
					Device: "Y2", NetworkConnected: true, PublicIP: "", Status: "ok",
				})
			} else {
				_ = json.NewEncoder(w).Encode(NetworkStatus{
					Device: "Y2", NetworkConnected: false, Status: "ok",
				})
			}
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "user", "pass", 10*time.Second)
	status, err := c.RestartDevice(context.Background(), "Y2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.NetworkConnected {
		t.Fatal("expected network_connected true after restart")
	}
	if loginCalls.Load() != 1 {
		t.Fatalf("expected 1 login call, got %d", loginCalls.Load())
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 PATCH calls, got %d", len(calls))
	}
}

func TestGetNetworkStatusReusesToken(t *testing.T) {
	var loginCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			loginCalls.Add(1)
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token123",
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer token123" {
			t.Fatalf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(NetworkStatus{
			Device: "Y2", NetworkConnected: true, PublicIP: "203.0.113.5", Status: "ok",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "user", "pass", 10*time.Second)
	if _, err := c.GetNetworkStatus(context.Background(), "Y2"); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if _, err := c.GetNetworkStatus(context.Background(), "Y2"); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if loginCalls.Load() != 1 {
		t.Fatalf("expected 1 login call, got %d", loginCalls.Load())
	}
}

func TestConcurrentRequestsShareSingleLogin(t *testing.T) {
	var loginCalls atomic.Int32
	loginStarted := make(chan struct{})
	releaseLogin := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			if loginCalls.Add(1) == 1 {
				close(loginStarted)
			}
			<-releaseLogin
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "shared-token",
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer shared-token" {
			t.Errorf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(NetworkStatus{Device: "Y2", NetworkConnected: true, Status: "ok"})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "user", "pass", 10*time.Second)
	const parallel = 8
	errors := make(chan error, parallel)
	var wg sync.WaitGroup
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.GetNetworkStatus(context.Background(), "Y2")
			errors <- err
		}()
	}
	<-loginStarted
	close(releaseLogin)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}
	if loginCalls.Load() != 1 {
		t.Fatalf("expected 1 login call, got %d", loginCalls.Load())
	}
}

func TestTokenRefreshedWhenExpired(t *testing.T) {
	var tokens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			tokens.Add(1)
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(-time.Second), Status: "ok", Token: "token-" + strconv.Itoa(int(tokens.Load())),
			})
			return
		}
		auth := r.Header.Get("Authorization")
		expected := "Bearer token-" + strconv.Itoa(int(tokens.Load()))
		if auth != expected {
			t.Fatalf("auth=%q, want %q", auth, expected)
		}
		_ = json.NewEncoder(w).Encode(NetworkStatus{
			Device: "Y2", NetworkConnected: true, PublicIP: "203.0.113.5", Status: "ok",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "user", "pass", 10*time.Second)
	if _, err := c.GetNetworkStatus(context.Background(), "Y2"); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	// The token expired immediately; the next call must log in again.
	if _, err := c.GetNetworkStatus(context.Background(), "Y2"); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if tokens.Load() != 2 {
		t.Fatalf("expected 2 login calls, got %d", tokens.Load())
	}
}

func TestUnauthorizedClearsTokenAndRetries(t *testing.T) {
	var tokens atomic.Int32
	var getCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			tokens.Add(1)
			_ = json.NewEncoder(w).Encode(LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "token-" + strconv.Itoa(int(tokens.Load())),
			})
			return
		}
		getCalls.Add(1)
		if getCalls.Load() == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		auth := r.Header.Get("Authorization")
		expected := "Bearer token-2"
		if auth != expected {
			t.Fatalf("auth=%q, want %q", auth, expected)
		}
		_ = json.NewEncoder(w).Encode(NetworkStatus{
			Device: "Y2", NetworkConnected: true, PublicIP: "203.0.113.5", Status: "ok",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "user", "pass", 10*time.Second)
	status, err := c.GetNetworkStatus(context.Background(), "Y2")
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if status.PublicIP != "203.0.113.5" {
		t.Fatalf("unexpected public ip: %s", status.PublicIP)
	}
	if tokens.Load() != 2 {
		t.Fatalf("expected 2 login calls, got %d", tokens.Load())
	}
}
