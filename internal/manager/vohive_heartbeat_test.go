package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

func TestVohiveHeartbeatRecordsDegradedAndRecovered(t *testing.T) {
	callCount := 0
	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			_ = json.NewEncoder(w).Encode(vohive.LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "test-token",
			})
			return
		}
		callCount++
		y2 := vohive.DeviceHealth{Healthy: true, ModemOK: true, IfaceUp: true, NetworkConnected: true, Signal: 80}
		if callCount >= 2 {
			y2.Healthy = false
			y2.NetworkConnected = false
		}
		_ = json.NewEncoder(w).Encode(vohive.HealthResponse{
			Status: "ok",
			Devices: map[string]vohive.DeviceHealth{
				"Y2": y2,
			},
		})
	}))
	defer vohiveServer.Close()

	m, st, _, cleanup := newVohiveTestManagerWithURL(t, vohiveServer.URL)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	m.runOneVohiveHeartbeatTick(context.Background())
	m.runOneVohiveHeartbeatTick(context.Background())

	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: "Y2"})
	if err != nil {
		t.Fatal(err)
	}
	var degradedCount int
	for _, e := range events {
		if e.Type == store.VohiveEventDegraded {
			degradedCount++
		}
	}
	if degradedCount != 1 {
		t.Fatalf("expected 1 degraded event for Y2, got %d", degradedCount)
	}
}

func TestVohiveHeartbeatFastMode(t *testing.T) {
	m, st, _, cleanup := newVohiveTestManagerWithURL(t, "http://127.0.0.1:1")
	defer cleanup()
	defer st.Close()
	defer m.Close()

	m.vohiveHealth = &vohiveHealthState{}
	m.setVohiveFastMode(true)
	if !m.vohiveHealth.fastMode() {
		t.Fatal("expected fastMode to be true after setVohiveFastMode(true)")
	}
	m.vohiveHealth.mu.Lock()
	m.vohiveHealth.fastUntil = time.Now().Add(-time.Millisecond)
	m.vohiveHealth.mu.Unlock()
	if m.vohiveHealth.fastMode() {
		t.Fatal("expected fastMode to be false after fastUntil expired")
	}
}

func TestVohiveHeartbeatReloadsOnRecovery(t *testing.T) {
	callCount := 0
	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			_ = json.NewEncoder(w).Encode(vohive.LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "test-token",
			})
			return
		}
		callCount++
		y2 := vohive.DeviceHealth{Healthy: false, ModemOK: true, IfaceUp: true, NetworkConnected: false, Signal: 60}
		if callCount >= 2 {
			y2.Healthy = true
			y2.NetworkConnected = true
		}
		_ = json.NewEncoder(w).Encode(vohive.HealthResponse{
			Status: "ok",
			Devices: map[string]vohive.DeviceHealth{
				"Y2": y2,
			},
		})
	}))
	defer vohiveServer.Close()

	m, st, cfg, cleanup := newVohiveTestManagerWithURL(t, vohiveServer.URL)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	var reloadCount int
	m.vohiveReload = func(_ context.Context, id int64) error {
		if id == cfg.ID {
			reloadCount++
		}
		return nil
	}

	m.runOneVohiveHeartbeatTick(context.Background())
	m.runOneVohiveHeartbeatTick(context.Background())

	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: "Y2"})
	if err != nil {
		t.Fatal(err)
	}
	var recoveredCount int
	for _, e := range events {
		if e.Type == store.VohiveEventRecovered {
			recoveredCount++
		}
	}
	if recoveredCount != 1 {
		t.Fatalf("expected 1 recovered event for Y2, got %d", recoveredCount)
	}
	if reloadCount != 1 {
		t.Fatalf("expected 1 reload for server %d, got %d", cfg.ID, reloadCount)
	}
}
