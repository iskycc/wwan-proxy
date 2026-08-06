package manager

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

func TestVohiveRecoveryTriggerRespectsThresholdAndCooldown(t *testing.T) {
	m, st, cfg, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	var calls int
	done := make(chan struct{}, 1)
	m.vohiveRecovery = func(context.Context, *instance, string) error {
		calls++
		done <- struct{}{}
		return nil
	}

	inst := m.instances[cfg.ID]
	if inst == nil {
		t.Fatal("instance not created")
	}

	waitForRecovery := func() {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("recovery goroutine did not run")
		}
	}

	// Below threshold: no recovery.
	m.maybeTriggerVohiveRecovery(inst, 1)
	if calls != 0 {
		t.Fatalf("recovery triggered below threshold, calls=%d", calls)
	}

	// At threshold: recovery triggered.
	m.maybeTriggerVohiveRecovery(inst, 2)
	waitForRecovery()
	if calls != 1 {
		t.Fatalf("recovery not triggered at threshold, calls=%d", calls)
	}

	// Cooldown still active: no second recovery.
	m.maybeTriggerVohiveRecovery(inst, 2)
	if calls != 1 {
		t.Fatalf("recovery triggered during cooldown, calls=%d", calls)
	}

	// Past cooldown: recovery triggered again.
	inst.mu.Lock()
	inst.lastVohiveAttempt = time.Now().Add(-10 * time.Minute)
	inst.mu.Unlock()
	m.maybeTriggerVohiveRecovery(inst, 2)
	waitForRecovery()
	if calls != 2 {
		t.Fatalf("recovery not triggered after cooldown, calls=%d", calls)
	}
}

func TestVohiveRecoverySkippedWithoutClient(t *testing.T) {
	m, st, cfg, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	var calls int
	m.vohiveRecovery = func(context.Context, *instance, string) error { calls++; return nil }

	inst := m.instances[cfg.ID]
	inst.vohiveClient = nil

	m.maybeTriggerVohiveRecovery(inst, 2)
	if calls != 0 {
		t.Fatalf("recovery triggered without vohive client, calls=%d", calls)
	}
}

func TestVohiveRecoveryFullFlowRestartsDeviceAndReloadsInstance(t *testing.T) {
	var mu sync.Mutex
	var requests []requestRecord
	statusPublicIP := ""
	statusNetworkConnected := false

	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, requestRecord{method: r.Method, path: r.URL.Path})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path == "/api/auth/login" {
				_ = json.NewEncoder(w).Encode(vohive.LoginResponse{
					ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "test-token",
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			enabled, _ := body["enabled"].(bool)
			if enabled {
				statusNetworkConnected = true
				statusPublicIP = "" // first enable returns empty public_ip
			} else {
				statusNetworkConnected = false
			}
			_ = json.NewEncoder(w).Encode(vohive.NetworkStatus{
				Device: "Y2", NetworkConnected: statusNetworkConnected, PublicIP: statusPublicIP, Status: "ok",
			})
		case http.MethodGet:
			// Second status check returns the public IP.
			if statusNetworkConnected {
				statusPublicIP = "203.0.113.5"
			}
			_ = json.NewEncoder(w).Encode(vohive.NetworkStatus{
				Device: "Y2", NetworkConnected: statusNetworkConnected, PublicIP: statusPublicIP, Status: "ok",
			})
		}
	}))
	defer vohiveServer.Close()

	m, st, cfg, cleanup := newVohiveTestManagerWithURL(t, vohiveServer.URL)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	var reloaded sync.WaitGroup
	reloaded.Add(1)
	originalReload := m.vohiveReload
	m.vohiveReload = func(ctx context.Context, id int64) error {
		defer reloaded.Done()
		return originalReload(ctx, id)
	}

	inst := m.instances[cfg.ID]
	if inst == nil {
		t.Fatal("instance not created")
	}

	// Override the post-reload sleep and status polling interval to keep the test fast.
	m.vohivePostRestartSleep = func(ctx context.Context) error { return nil }
	m.vohiveStatusRetryDelay = func(ctx context.Context) error { return nil }

	m.runVohiveRecovery(context.Background(), inst, cfg.VohiveDeviceID)

	reloaded.Wait()

	// Verify recovery events were persisted.
	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: cfg.VohiveDeviceID})
	if err != nil {
		t.Fatal(err)
	}
	var startedCount, succeededCount int
	for _, e := range events {
		if e.DeviceID != cfg.VohiveDeviceID {
			continue
		}
		switch e.Type {
		case store.VohiveEventRecoveryStarted:
			startedCount++
		case store.VohiveEventRecoverySucceeded:
			succeededCount++
			if e.ServerID == nil || *e.ServerID != cfg.ID {
				t.Fatalf("recovery_succeeded event has wrong server_id: got %v, want %d", e.ServerID, cfg.ID)
			}
			if e.DeviceID != cfg.VohiveDeviceID {
				t.Fatalf("recovery_succeeded event has wrong device_id: got %q, want %q", e.DeviceID, cfg.VohiveDeviceID)
			}
		}
	}
	if startedCount != 1 {
		t.Fatalf("expected 1 recovery_started event, got %d", startedCount)
	}
	if succeededCount != 1 {
		t.Fatalf("expected 1 recovery_succeeded event, got %d", succeededCount)
	}

	mu.Lock()
	defer mu.Unlock()

	want := []requestRecord{
		{method: http.MethodPost, path: "/api/auth/login"},
		{method: http.MethodPatch, path: "/api/devices/Y2/network"},
		{method: http.MethodPatch, path: "/api/devices/Y2/network"},
		{method: http.MethodGet, path: "/api/devices/Y2/network"},
	}
	if len(requests) < len(want) {
		t.Fatalf("expected at least %d requests, got %d: %+v", len(want), len(requests), requests)
	}
	for i, r := range want {
		if requests[i] != r {
			t.Fatalf("request %d = %+v, want %+v", i, requests[i], r)
		}
	}
	if statusPublicIP != "203.0.113.5" {
		t.Fatalf("public_ip was not confirmed, got %q", statusPublicIP)
	}
}

func TestVohiveRecoveryRetriesStatusUntilPublicIP(t *testing.T) {
	callCount := 0
	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			_ = json.NewEncoder(w).Encode(vohive.LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "test-token",
			})
			return
		}
		if r.Method == http.MethodPatch {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			enabled, _ := body["enabled"].(bool)
			connected := enabled
			_ = json.NewEncoder(w).Encode(vohive.NetworkStatus{
				Device: "Y2", NetworkConnected: connected, PublicIP: "", Status: "ok",
			})
			return
		}
		callCount++
		publicIP := ""
		if callCount >= 3 {
			publicIP = "203.0.113.7"
		}
		_ = json.NewEncoder(w).Encode(vohive.NetworkStatus{
			Device: "Y2", NetworkConnected: true, PublicIP: publicIP, Status: "ok",
		})
	}))
	defer vohiveServer.Close()

	m, st, cfg, cleanup := newVohiveTestManagerWithURL(t, vohiveServer.URL)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	m.vohiveReload = func(context.Context, int64) error { return nil }
	m.vohivePostRestartSleep = func(context.Context) error { return nil }
	m.vohiveStatusRetryDelay = func(context.Context) error { return nil }

	inst := m.instances[cfg.ID]
	m.runVohiveRecovery(context.Background(), inst, cfg.VohiveDeviceID)

	if callCount != 3 {
		t.Fatalf("expected 3 status checks, got %d", callCount)
	}
}

func TestVohiveRecoveryFailureRecordsRecoveryFailedEvent(t *testing.T) {
	m, st, cfg, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	inst := m.instances[cfg.ID]
	if inst == nil {
		t.Fatal("instance not created")
	}
	inst.mu.Lock()
	inst.vohiveInProgress = false
	inst.lastVohiveAttempt = time.Time{}
	inst.mu.Unlock()

	m.runVohiveRecovery(context.Background(), inst, cfg.VohiveDeviceID)

	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: cfg.VohiveDeviceID})
	if err != nil {
		t.Fatal(err)
	}
	var started, failed int
	for _, e := range events {
		if e.ServerID != nil && *e.ServerID == cfg.ID && e.DeviceID == cfg.VohiveDeviceID {
			switch e.Type {
			case store.VohiveEventRecoveryStarted:
				started++
			case store.VohiveEventRecoveryFailed:
				failed++
			}
		}
	}
	if started != 1 {
		t.Fatalf("expected 1 recovery_started event, got %d", started)
	}
	if failed != 1 {
		t.Fatalf("expected 1 recovery_failed event, got %d", failed)
	}
}

type requestRecord struct {
	method string
	path   string
}

func newVohiveTestManager(t *testing.T) (*Manager, *store.Store, config.Server, func()) {
	return newVohiveTestManagerWithURL(t, "http://127.0.0.1:1")
}

func newVohiveTestManagerWithURL(t *testing.T, vohiveURL string) (*Manager, *store.Store, config.Server, func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/vohive.db")
	if err != nil {
		t.Fatal(err)
	}

	heartbeat := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	cfg := config.Server{
		Enabled: true, Name: "vohive-test", Listen: unusedTCPAddress(t), Interface: "lo",
		Auth: config.Auth{Method: "none"},
		Heartbeat: config.Heartbeat{
			URL: heartbeat.URL, Interval: config.Duration(5 * time.Second), Timeout: config.Duration(time.Second),
		},
		VohiveDeviceID: "Y2",
	}
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}

	settings := config.SystemSettings{
		WebListen: "127.0.0.1:9090",
		Vohive: config.VohiveSettings{
			Enabled: true, BaseURL: vohiveURL, Username: "user", Password: "pass",
			ConsecutiveFailures: 2, Cooldown: config.Duration(5 * time.Minute),
		},
	}
	if err := st.SaveSystemSettings(context.Background(), &settings); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(context.Background(), st, logger)
	m.preflightDevice = func(string) error { return nil }

	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	cleanup := func() {
		heartbeat.Close()
		m.Close()
		st.Close()
	}
	return m, st, cfg, cleanup
}
