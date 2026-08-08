package manager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

func TestVohiveHeartbeatRecordsDegradedAndRecovered(t *testing.T) {
	callCount := 0
	loginCount := 0
	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			loginCount++
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
	for _, inst := range m.instances {
		if inst.vohiveClient != m.vohiveHealth.client {
			t.Fatal("heartbeat and per-instance recovery must share one Vohive token cache")
		}
	}

	m.runOneVohiveHeartbeatTick(context.Background())
	m.runOneVohiveHeartbeatTick(context.Background())
	if loginCount != 1 {
		t.Fatalf("expected heartbeat client to reuse one login token, got %d logins", loginCount)
	}

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

func TestVohiveHeartbeatFastModeRefCount(t *testing.T) {
	m, st, _, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	m.vohiveHealth = &vohiveHealthState{}

	m.enterVohiveFastMode()
	m.enterVohiveFastMode()
	if !m.vohiveHealth.fastMode() {
		t.Fatal("expected fastMode true after enters")
	}
	m.vohiveHealth.mu.RLock()
	if m.vohiveHealth.fastRefCount != 2 {
		t.Fatalf("expected fastRefCount=2, got %d", m.vohiveHealth.fastRefCount)
	}
	m.vohiveHealth.mu.RUnlock()

	m.leaveVohiveFastMode()
	if !m.vohiveHealth.fastMode() {
		t.Fatal("expected fastMode still true after one leave with refcount=1")
	}

	m.leaveVohiveFastMode()
	if !m.vohiveHealth.fastMode() {
		t.Fatal("expected fastMode true immediately after last leave due to 30s grace")
	}
	m.vohiveHealth.mu.RLock()
	if m.vohiveHealth.fastRefCount != 0 {
		t.Fatalf("expected fastRefCount=0, got %d", m.vohiveHealth.fastRefCount)
	}
	m.vohiveHealth.mu.RUnlock()

	// Expire the grace period.
	m.vohiveHealth.mu.Lock()
	m.vohiveHealth.fastUntil = time.Now().Add(-time.Millisecond)
	m.vohiveHealth.mu.Unlock()
	if m.vohiveHealth.fastMode() {
		t.Fatal("expected fastMode false after grace period expired")
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

func TestHeartbeatFailureEntersVohiveFastMode(t *testing.T) {
	m, st, cfg, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	inst := m.instances[cfg.ID]
	if inst == nil {
		t.Fatal("instance not created")
	}
	inst.heartbeatCancel()
	inst.cancel()
	inst.wg.Wait()

	m.heartbeatCheckResult(inst, heartbeatProbeResult{
		heartbeat:    store.Heartbeat{Healthy: false, Error: "probe failure"},
		failureStage: "http_status",
		cause:        errors.New("boom"),
	})

	if !m.vohiveHealth.fastMode() {
		t.Fatal("expected vohive fastMode to be true after heartbeat failure")
	}
	m.vohiveHealth.mu.RLock()
	refCount := m.vohiveHealth.fastRefCount
	m.vohiveHealth.mu.RUnlock()
	if refCount != 1 {
		t.Fatalf("expected fastRefCount=1, got %d", refCount)
	}
}

func TestHeartbeatRecoveryLeavesVohiveFastModeAndReloads(t *testing.T) {
	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			_ = json.NewEncoder(w).Encode(vohive.LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "test-token",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(vohive.HealthResponse{
			Status: "ok",
			Devices: map[string]vohive.DeviceHealth{
				"Y2": {Healthy: true, ModemOK: true, IfaceUp: true, NetworkConnected: true, Signal: 80},
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

	inst := m.instances[cfg.ID]
	if inst == nil {
		t.Fatal("instance not created")
	}
	inst.heartbeatCancel()
	inst.cancel()
	inst.wg.Wait()

	m.heartbeatCheckResult(inst, heartbeatProbeResult{
		heartbeat:    store.Heartbeat{Healthy: false, Error: "probe failure"},
		failureStage: "http_status",
		cause:        errors.New("boom"),
	})
	m.heartbeatCheckResult(inst, heartbeatProbeResult{
		heartbeat: store.Heartbeat{Healthy: true, PublicIP: "203.0.113.8"},
	})

	if reloadCount != 1 {
		t.Fatalf("expected 1 reload after heartbeat recovery, got %d", reloadCount)
	}

	m.vohiveHealth.mu.Lock()
	m.vohiveHealth.fastUntil = time.Now().Add(-time.Millisecond)
	m.vohiveHealth.mu.Unlock()

	if m.vohiveHealth.fastMode() {
		t.Fatal("expected vohive fastMode to be false after grace period expired")
	}
	m.vohiveHealth.mu.RLock()
	refCount := m.vohiveHealth.fastRefCount
	m.vohiveHealth.mu.RUnlock()
	if refCount != 0 {
		t.Fatalf("expected fastRefCount=0, got %d", refCount)
	}
}

func TestVohiveHealthFailureRecordsEvent(t *testing.T) {
	m, st, _, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	m.runOneVohiveHeartbeatTick(context.Background())

	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: "system"})
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
		t.Fatalf("expected 1 degraded event for system, got %d", degradedCount)
	}
}

func TestVohiveHealthFailureRecordsDeviceSummaryAndRawError(t *testing.T) {
	vohiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/auth/login" {
			_ = json.NewEncoder(w).Encode(vohive.LoginResponse{
				ExpiresAt: time.Now().Add(time.Hour), Status: "ok", Token: "test-token",
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(vohive.HealthResponse{
			Status: "unhealthy",
			Devices: map[string]vohive.DeviceHealth{
				"Y1": {Healthy: true, ModemOK: true, IfaceUp: true, NetworkConnected: true, Signal: -53},
				"Y2": {Healthy: true, ModemOK: true, Signal: -54},
				"Y3": {Healthy: true, ModemOK: true, Signal: -56},
				"Y4": {Healthy: false, ModemOK: false},
			},
		})
	}))
	defer vohiveServer.Close()

	m, st, _, cleanup := newVohiveTestManagerWithURL(t, vohiveServer.URL)
	defer cleanup()

	m.runOneVohiveHeartbeatTick(context.Background())
	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Message != "vohive health unhealthy: healthy Y1, Y2, Y3; unhealthy Y4" {
		t.Fatalf("message = %q", event.Message)
	}
	if strings.Contains(event.Message, `"devices"`) {
		t.Fatal("summary must not contain the raw health response")
	}
	rawError, _ := event.Details["error"].(string)
	if !strings.Contains(rawError, "returned 503") || !strings.Contains(rawError, `"Y4"`) {
		t.Fatalf("raw error details = %q", rawError)
	}
	if _, ok := event.Details["devices"].(map[string]any); !ok {
		t.Fatalf("structured devices missing from details: %+v", event.Details)
	}
}

func TestManagerCloseWaitsForVohiveRecovery(t *testing.T) {
	m, st, cfg, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()

	inst := m.instances[cfg.ID]
	if inst == nil {
		t.Fatal("instance not created")
	}

	done := make(chan struct{})
	m.vohiveRecovery = func(ctx context.Context, inst *instance, deviceID string) error {
		<-ctx.Done()
		close(done)
		return ctx.Err()
	}

	inst.mu.Lock()
	inst.vohiveInProgress = false
	inst.lastVohiveAttempt = time.Time{}
	inst.mu.Unlock()

	m.maybeTriggerVohiveRecovery(inst, 2)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		m.Close()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery goroutine was not canceled")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish")
	}
}

func TestVohiveHeartbeatShutdownDoesNotRecordFalseDegradedEvent(t *testing.T) {
	m, st, _, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()

	m.Close()

	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{DeviceID: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no system degraded events after shutdown, got %+v", events)
	}
}
