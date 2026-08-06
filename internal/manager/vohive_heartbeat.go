package manager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

type vohiveHealthState struct {
	mu           sync.RWMutex
	devices      map[string]vohive.DeviceHealth
	fastUntil    time.Time
	fastRefCount int
	lastError    string
}

func (m *Manager) startVohiveHeartbeat(settings config.VohiveSettings) {
	if !settings.Enabled || settings.BaseURL == "" {
		return
	}
	state := &vohiveHealthState{}
	m.vohiveHealth = state

	ctx, cancel := context.WithCancel(m.ctx)
	m.vohiveHeartbeatCancel = cancel
	m.vohiveHeartbeatWG.Add(1)
	go func() {
		defer m.vohiveHeartbeatWG.Done()
		ticker := time.NewTicker(m.vohiveInterval(false))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.runOneVohiveHeartbeatTick(ctx)
				newInterval := m.vohiveInterval(state.fastMode())
				ticker.Reset(newInterval)
			}
		}
	}()
}

func (m *Manager) runOneVohiveHeartbeatTick(ctx context.Context) {
	if m.vohiveHealth == nil {
		return
	}
	settings := m.vohiveSettings()
	client := vohive.NewClient(settings.BaseURL, settings.Username, settings.Password, 30*time.Second)
	health, err := client.GetHealth(ctx)
	if err != nil {
		m.vohiveHealth.mu.Lock()
		m.vohiveHealth.lastError = err.Error()
		m.vohiveHealth.mu.Unlock()
		m.log.Error("vohive health check failed", "error", err)
		m.recordVohiveEvent(ctx, store.VohiveEventDegraded, "system", nil,
			fmt.Sprintf("vohive health check failed: %v", err),
			map[string]any{"error": err.Error()})
		return
	}
	m.vohiveHealth.mu.Lock()
	m.vohiveHealth.lastError = ""
	previous := m.vohiveHealth.devices
	m.vohiveHealth.devices = health.Devices
	m.vohiveHealth.mu.Unlock()

	for id, dh := range health.Devices {
		prev, hadPrev := previous[id]
		if !hadPrev {
			continue
		}
		if prev.Healthy && !dh.Healthy {
			m.recordVohiveEvent(ctx, store.VohiveEventDegraded, id, nil, fmt.Sprintf("device %s became unhealthy", id), deviceHealthDetails(dh))
		} else if !prev.Healthy && dh.Healthy {
			m.recordVohiveEvent(ctx, store.VohiveEventRecovered, id, nil, fmt.Sprintf("device %s recovered", id), deviceHealthDetails(dh))
			m.reloadServersForDevice(ctx, id)
		}
	}
}

func (m *Manager) vohiveInterval(fast bool) time.Duration {
	if fast {
		return 5 * time.Second
	}
	return 30 * time.Second
}

func (s *vohiveHealthState) fastMode() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fastRefCount > 0 || time.Until(s.fastUntil) > 0
}

func (m *Manager) setVohiveFastMode(fast bool) {
	if m.vohiveHealth == nil {
		return
	}
	m.vohiveHealth.mu.Lock()
	defer m.vohiveHealth.mu.Unlock()
	if fast {
		m.vohiveHealth.fastUntil = time.Now().Add(5 * time.Minute)
	} else {
		m.vohiveHealth.fastUntil = time.Time{}
		m.vohiveHealth.fastRefCount = 0
	}
}

func (m *Manager) enterVohiveFastMode() {
	if m.vohiveHealth == nil {
		return
	}
	m.vohiveHealth.mu.Lock()
	m.vohiveHealth.fastRefCount++
	m.vohiveHealth.mu.Unlock()
}

func (m *Manager) leaveVohiveFastMode() {
	if m.vohiveHealth == nil {
		return
	}
	m.vohiveHealth.mu.Lock()
	if m.vohiveHealth.fastRefCount > 0 {
		m.vohiveHealth.fastRefCount--
	}
	if m.vohiveHealth.fastRefCount == 0 {
		// Keep fast mode alive for a 30 s grace period after the last failure.
		m.vohiveHealth.fastUntil = time.Now().Add(30 * time.Second)
	}
	m.vohiveHealth.mu.Unlock()
}

func (m *Manager) vohiveSettings() config.VohiveSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.systemVohiveSettings
}

func (m *Manager) reloadServersForDevice(ctx context.Context, deviceID string) {
	m.mu.RLock()
	instances := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		if inst.cfg.VohiveDeviceID == deviceID {
			instances = append(instances, inst)
		}
	}
	m.mu.RUnlock()
	for _, inst := range instances {
		if err := m.vohiveReload(ctx, inst.cfg.ID); err != nil {
			m.log.Error("failed to reload server after vohive recovery", "server", inst.cfg.Name, "error", err)
		}
	}
}

func (m *Manager) recordVohiveEvent(ctx context.Context, typ store.VohiveEventType, deviceID string, serverID *int64, message string, details map[string]any) {
	_, err := m.store.SaveVohiveEvent(ctx, store.VohiveEvent{
		Type:      typ,
		DeviceID:  deviceID,
		ServerID:  serverID,
		Message:   message,
		Details:   details,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		m.log.Error("failed to save vohive event", "type", typ, "device", deviceID, "error", err)
	}
}

func deviceHealthDetails(d vohive.DeviceHealth) map[string]any {
	return map[string]any{
		"healthy":           d.Healthy,
		"modem_ok":          d.ModemOK,
		"iface_up":          d.IfaceUp,
		"network_connected": d.NetworkConnected,
		"signal":            d.Signal,
	}
}
