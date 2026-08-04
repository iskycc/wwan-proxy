package manager

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/httpproxy"
	"wwan-proxy/internal/socks5"
	"wwan-proxy/internal/store"
)

type Manager struct {
	store *store.Store
	log   *slog.Logger
	ctx   context.Context

	mu        sync.RWMutex
	instances map[int64]*instance
}

type instance struct {
	cfg       config.Server
	server    *socks5.Server
	httpProxy *httpproxy.Server
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startedAt time.Time

	mu           sync.RWMutex
	socksRunning bool
	httpRunning  bool
	lastError    string
}

type InstanceSnapshot struct {
	ID          int64                     `json:"id"`
	Name        string                    `json:"name"`
	Enabled     bool                      `json:"enabled"`
	Running     bool                      `json:"running"`
	Listen      string                    `json:"listen"`
	Interface   string                    `json:"interface"`
	StartedAt   time.Time                 `json:"started_at,omitempty"`
	LastError   string                    `json:"last_error,omitempty"`
	Metrics     socks5.MetricsSnapshot    `json:"metrics"`
	HTTPListen  string                    `json:"http_listen,omitempty"`
	HTTPRunning bool                      `json:"http_running"`
	HTTPMetrics httpproxy.MetricsSnapshot `json:"http_metrics"`
}

func New(ctx context.Context, st *store.Store, logger *slog.Logger) *Manager {
	return &Manager{store: st, log: logger.With("component", "manager"), ctx: ctx, instances: make(map[int64]*instance)}
}

func (m *Manager) StartAll(ctx context.Context) error {
	configs, err := m.store.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range configs {
		if cfg.Enabled {
			m.start(cfg)
		}
	}
	return nil
}

func (m *Manager) Reload(ctx context.Context, id int64) error {
	m.stop(id)
	cfg, err := m.store.GetServer(ctx, id)
	if err != nil {
		return err
	}
	if cfg.Enabled {
		m.start(cfg)
	}
	return nil
}

func (m *Manager) Remove(id int64) { m.stop(id) }

func (m *Manager) Close() {
	m.mu.RLock()
	ids := make([]int64, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.stop(id)
	}
}

func (m *Manager) start(cfg config.Server) {
	ctx, cancel := context.WithCancel(m.ctx)
	srv := socks5.New(cfg, m.log)
	var httpProxy *httpproxy.Server
	if cfg.HTTPProxy.Enabled {
		dialer := srv.OutboundDialer()
		httpProxy = httpproxy.New(cfg, m.log, dialer.DialContext)
	}
	inst := &instance{cfg: cfg, server: srv, httpProxy: httpProxy, cancel: cancel, startedAt: time.Now(), socksRunning: true, httpRunning: cfg.HTTPProxy.Enabled}
	m.mu.Lock()
	if old := m.instances[cfg.ID]; old != nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.instances[cfg.ID] = inst
	m.mu.Unlock()

	if iface, err := net.InterfaceByName(cfg.Interface); err != nil {
		inst.setError(fmt.Sprintf("interface lookup: %v", err))
		m.log.Error("configured interface is unavailable", "server", cfg.Name, "interface", cfg.Interface, "error", err)
	} else if iface.Flags&net.FlagUp == 0 {
		inst.setError("interface is down")
		m.log.Error("configured interface is down", "server", cfg.Name, "interface", cfg.Interface)
	}
	inst.wg.Add(2)
	go func() {
		defer inst.wg.Done()
		err := srv.ListenAndServe(ctx)
		inst.mu.Lock()
		inst.socksRunning = false
		if err != nil {
			inst.lastError = err.Error()
		}
		inst.mu.Unlock()
		if err != nil {
			m.log.Error("SOCKS5 listener stopped", "server", cfg.Name, "listen", cfg.Listen, "interface", cfg.Interface, "error", err)
		}
	}()
	if httpProxy != nil {
		inst.wg.Add(1)
		go func() {
			defer inst.wg.Done()
			err := httpProxy.ListenAndServe(ctx)
			inst.mu.Lock()
			inst.httpRunning = false
			if err != nil {
				inst.lastError = "HTTP proxy: " + err.Error()
			}
			inst.mu.Unlock()
			if err != nil {
				m.log.Error("HTTP/HTTPS proxy listener stopped", "server", cfg.Name, "listen", cfg.HTTPProxy.Listen, "interface", cfg.Interface, "error", err)
			}
		}()
	}
	go func() { defer inst.wg.Done(); m.heartbeatLoop(ctx, inst) }()
}

func (m *Manager) stop(id int64) {
	m.mu.Lock()
	inst := m.instances[id]
	delete(m.instances, id)
	m.mu.Unlock()
	if inst == nil {
		return
	}
	inst.cancel()
	_ = inst.server.Close()
	if inst.httpProxy != nil {
		_ = inst.httpProxy.Close()
	}
	inst.wg.Wait()
	m.log.Info("server instance stopped", "server", inst.cfg.Name, "interface", inst.cfg.Interface)
}

func (m *Manager) Snapshots() []InstanceSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]InstanceSnapshot, 0, len(m.instances))
	for _, inst := range m.instances {
		inst.mu.RLock()
		httpMetrics := httpproxy.MetricsSnapshot{}
		if inst.httpProxy != nil {
			httpMetrics = inst.httpProxy.Metrics()
		}
		running := inst.socksRunning && (!inst.cfg.HTTPProxy.Enabled || inst.httpRunning)
		snap := InstanceSnapshot{ID: inst.cfg.ID, Name: inst.cfg.Name, Enabled: true, Running: running, Listen: inst.cfg.Listen, Interface: inst.cfg.Interface, StartedAt: inst.startedAt, LastError: inst.lastError, Metrics: inst.server.Metrics(), HTTPListen: inst.cfg.HTTPProxy.Listen, HTTPRunning: inst.httpRunning, HTTPMetrics: httpMetrics}
		inst.mu.RUnlock()
		result = append(result, snap)
	}
	return result
}

func (i *instance) setError(message string) { i.mu.Lock(); i.lastError = message; i.mu.Unlock() }

func (i *instance) clearHeartbeatError() {
	i.mu.Lock()
	if len(i.lastError) >= len("heartbeat:") && i.lastError[:len("heartbeat:")] == "heartbeat:" {
		i.lastError = ""
	}
	i.mu.Unlock()
}
