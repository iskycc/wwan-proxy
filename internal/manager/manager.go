package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/httpproxy"
	"wwan-proxy/internal/policy"
	"wwan-proxy/internal/socks5"
	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

type Manager struct {
	store *store.Store
	log   *slog.Logger
	ctx   context.Context

	mu              sync.RWMutex
	instances       map[int64]*instance
	transition      sync.Mutex
	drainMu         sync.Mutex
	draining        map[*instance]context.CancelFunc
	drainWG         sync.WaitGroup
	preflightDevice func(string) error

	vohiveRecovery         func(context.Context, *instance, string) error
	vohiveReload           func(context.Context, int64) error
	vohivePostRestartSleep func(context.Context) error
	vohiveStatusRetryDelay func(context.Context) error

	vohiveHealth          *vohiveHealthState
	systemVohiveSettings  config.VohiveSettings
	vohiveHeartbeatCancel context.CancelFunc
	vohiveHeartbeatWG     sync.WaitGroup
}

type instance struct {
	cfg             config.Server
	server          *socks5.Server
	httpProxy       *httpproxy.Server
	ctx             context.Context
	cancel          context.CancelFunc
	heartbeatCtx    context.Context
	heartbeatCancel context.CancelFunc
	wg              sync.WaitGroup
	startedAt       time.Time
	limits          *instanceLimits
	vohiveClient    *vohive.Client
	vohiveSettings  config.VohiveSettings

	mu                sync.RWMutex
	launched          bool
	socksRunning      bool
	httpRunning       bool
	lastError         string
	startupErrors     chan error
	vohiveInProgress  bool
	lastVohiveAttempt time.Time
}

type instanceLimits struct {
	connections     *policy.Limiter
	clients         *policy.IPLimiter
	udpClients      *policy.IPLimiter
	udpAssociations *policy.Limiter
}

func (l *instanceLimits) setMax(cfg config.Server) {
	udpLimit := cfg.UDP.MaxAssociations
	if udpLimit == 0 {
		udpLimit = 64
	}
	l.connections.SetMax(cfg.MaxConnections)
	l.clients.SetMax(cfg.Access.MaxConnectionsPerIP)
	l.udpClients.SetMax(cfg.Access.MaxUDPAssociationsPerIP)
	l.udpAssociations.SetMax(udpLimit)
}

var (
	reloadStartupTimeout = 5 * time.Second
	reloadDrainTimeout   = 30 * time.Second
)

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
	m := &Manager{
		store: st, log: logger.With("component", "manager"), ctx: ctx,
		instances: make(map[int64]*instance), draining: make(map[*instance]context.CancelFunc),
		preflightDevice: defaultDevicePreflight,
	}
	m.vohiveReload = m.Reload
	m.vohivePostRestartSleep = func(ctx context.Context) error {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	m.vohiveStatusRetryDelay = func(ctx context.Context) error {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	m.vohiveRecovery = m.runVohiveRecovery
	return m
}

func (m *Manager) StartAll(ctx context.Context) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	configs, err := m.store.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range configs {
		if cfg.Enabled {
			m.start(cfg)
		}
	}
	if settings, err := m.store.SystemSettings(ctx); err == nil {
		m.mu.Lock()
		m.systemVohiveSettings = settings.Vohive
		m.mu.Unlock()
		m.startVohiveHeartbeat(settings.Vohive)
	} else {
		m.log.Error("failed to load system settings for vohive heartbeat", "error", err)
	}
	return nil
}

func (m *Manager) Reload(_ context.Context, id int64) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	loadCtx, loadCancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer loadCancel()
	cfg, err := m.store.GetServer(loadCtx, id)
	if err != nil {
		return err
	}

	m.mu.RLock()
	old := m.instances[id]
	m.mu.RUnlock()
	if cfg.Enabled {
		if err := m.checkDevicePreflight(cfg.Interface); err != nil {
			if old == nil {
				disabled := cfg
				disabled.Enabled = false
				if saveErr := m.saveRuntimeConfig(disabled); saveErr != nil {
					return fmt.Errorf("%w; disabling stored configuration failed: %v", err, saveErr)
				}
				return fmt.Errorf("%w; configuration disabled", err)
			}
			if saveErr := m.saveRuntimeConfig(old.cfg); saveErr != nil {
				return fmt.Errorf("%w; stored configuration recovery failed: %v", err, saveErr)
			}
			old.mu.RLock()
			oldLaunched := old.launched
			old.mu.RUnlock()
			if !oldLaunched {
				return fmt.Errorf("%w; previous stopped configuration restored", err)
			}
			return fmt.Errorf("%w; previous instance kept running", err)
		}
	}
	if old == nil {
		if !cfg.Enabled {
			return nil
		}
		replacement := m.newInstance(cfg, nil)
		m.mu.Lock()
		m.instances[id] = replacement
		m.mu.Unlock()
		m.launch(replacement)
		if err := m.waitReady(m.ctx, replacement); err != nil {
			m.shutdownInstance(replacement)
			m.mu.Lock()
			if m.instances[id] == replacement {
				delete(m.instances, id)
			}
			m.mu.Unlock()
			disabled := cfg
			disabled.Enabled = false
			if saveErr := m.saveRuntimeConfig(disabled); saveErr != nil {
				return fmt.Errorf("listener startup failed: %w; disabling stored configuration failed: %v", err, saveErr)
			}
			return fmt.Errorf("listener startup failed; configuration disabled: %w", err)
		}
		return nil
	}
	if !cfg.Enabled {
		m.mu.Lock()
		if m.instances[id] == old {
			delete(m.instances, id)
		}
		m.mu.Unlock()
		m.shutdownInstance(old)
		return nil
	}

	old.stopAccepting()
	// Once the old listeners stop accepting, their heartbeat is no longer the
	// canonical status for this server ID. Stop it before the replacement probe
	// can write to the same SQLite row.
	old.heartbeatCancel()
	replacement := m.newInstance(cfg, old.limits)
	m.mu.Lock()
	if m.instances[id] != old {
		m.mu.Unlock()
		m.shutdownInstance(replacement)
		return fmt.Errorf("server instance %d changed during reload", id)
	}
	m.instances[id] = replacement
	m.mu.Unlock()
	m.launch(replacement)
	if err := m.waitReady(m.ctx, replacement); err != nil {
		m.shutdownInstance(replacement)
		old.mu.RLock()
		oldLaunched := old.launched
		old.mu.RUnlock()
		if !oldLaunched {
			// A failed StartAll preflight leaves a stopped diagnostic
			// placeholder in the instance map. If a later reload passes
			// preflight but its listeners fail, restore that placeholder as-is;
			// attempting to launch its known-invalid configuration would turn
			// a safe rollback into a false Running state.
			old.limits.setMax(old.cfg)
			m.mu.Lock()
			if m.instances[id] == replacement {
				m.instances[id] = old
			}
			m.mu.Unlock()
			storeErr := m.saveRuntimeConfig(old.cfg)
			if storeErr != nil {
				return fmt.Errorf("reload failed: %w; previous stopped configuration restored in memory; stored configuration recovery failed: %v", err, storeErr)
			}
			return fmt.Errorf("reload failed, previous stopped configuration restored: %w", err)
		}
		rollback := m.newInstance(old.cfg, old.limits)
		m.mu.Lock()
		if m.instances[id] == replacement {
			m.instances[id] = rollback
		}
		m.mu.Unlock()
		m.launch(rollback)
		rollbackErr := m.waitReady(m.ctx, rollback)
		storeErr := m.saveRuntimeConfig(old.cfg)
		m.scheduleDrain(old)
		if rollbackErr != nil || storeErr != nil {
			return fmt.Errorf("reload failed: %w; previous listener recovery failed: %v; stored configuration recovery failed: %v", err, rollbackErr, storeErr)
		}
		return fmt.Errorf("reload failed, previous configuration restored: %w", err)
	}
	m.scheduleDrain(old)
	return nil
}

func (m *Manager) saveRuntimeConfig(cfg config.Server) error {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	snapshot := cfg.Clone()
	return m.store.SaveServer(ctx, &snapshot)
}

func (m *Manager) Remove(id int64) { m.stop(id) }

func (m *Manager) Close() {
	m.transition.Lock()
	defer m.transition.Unlock()
	m.mu.RLock()
	instances := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.RUnlock()
	m.mu.Lock()
	clear(m.instances)
	m.mu.Unlock()
	m.drainMu.Lock()
	for _, cancel := range m.draining {
		cancel()
	}
	m.drainMu.Unlock()
	for _, inst := range instances {
		m.shutdownInstance(inst)
	}
	m.drainWG.Wait()
	if m.vohiveHeartbeatCancel != nil {
		m.vohiveHeartbeatCancel()
	}
	m.vohiveHeartbeatWG.Wait()
}

func (m *Manager) start(cfg config.Server) {
	preflightErr := m.checkDevicePreflight(cfg.Interface)
	inst := m.newInstance(cfg, nil)
	if preflightErr != nil {
		inst.heartbeatCancel()
		inst.cancel()
		_ = inst.server.Close()
		if inst.httpProxy != nil {
			_ = inst.httpProxy.Close()
		}
		inst.mu.Lock()
		inst.socksRunning = false
		inst.httpRunning = false
		inst.lastError = preflightErr.Error()
		inst.mu.Unlock()
	}
	m.mu.Lock()
	if old := m.instances[cfg.ID]; old != nil {
		m.mu.Unlock()
		m.shutdownInstance(inst)
		return
	}
	m.instances[cfg.ID] = inst
	m.mu.Unlock()
	if preflightErr != nil {
		m.log.Error("server egress preflight failed", "server", cfg.Name, "interface", cfg.Interface, "error", preflightErr)
		return
	}
	m.launch(inst)
}

func defaultDevicePreflight(name string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface lookup: %w", err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("interface is down")
	}
	return socks5.PreflightDeviceBinding(name)
}

func (m *Manager) checkDevicePreflight(name string) error {
	check := m.preflightDevice
	if check == nil {
		check = defaultDevicePreflight
	}
	if err := check(name); err != nil {
		return fmt.Errorf("egress interface %q preflight failed: %w", name, err)
	}
	return nil
}

func (m *Manager) newInstance(cfg config.Server, limits *instanceLimits) *instance {
	ctx, cancel := context.WithCancel(m.ctx)
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	udpLimit := cfg.UDP.MaxAssociations
	if udpLimit == 0 {
		udpLimit = 64
	}
	if limits == nil {
		limits = &instanceLimits{
			connections:     policy.NewLimiter(cfg.MaxConnections),
			clients:         policy.NewIPLimiter(cfg.Access.MaxConnectionsPerIP),
			udpClients:      policy.NewIPLimiter(cfg.Access.MaxUDPAssociationsPerIP),
			udpAssociations: policy.NewLimiter(udpLimit),
		}
	} else {
		limits.setMax(cfg)
	}
	srv := socks5.NewWithAllLimiters(cfg, m.log, limits.connections, limits.clients, limits.udpClients, limits.udpAssociations)
	var httpProxy *httpproxy.Server
	if cfg.HTTPProxy.Enabled {
		httpProxy = httpproxy.NewWithLimiters(cfg, m.log, srv.DialContext, limits.connections, limits.clients)
	}
	inst := &instance{
		cfg: cfg, server: srv, httpProxy: httpProxy, ctx: ctx, cancel: cancel,
		heartbeatCtx: heartbeatCtx, heartbeatCancel: heartbeatCancel, limits: limits,
		socksRunning: true, httpRunning: cfg.HTTPProxy.Enabled, startupErrors: make(chan error, 4),
	}
	if settings, err := m.store.SystemSettings(ctx); err == nil && settings.Vohive.Enabled && cfg.VohiveDeviceID != "" {
		inst.vohiveSettings = settings.Vohive
		inst.vohiveClient = vohive.NewClient(settings.Vohive.BaseURL, settings.Vohive.Username, settings.Vohive.Password, 30*time.Second)
	}
	return inst
}

func (m *Manager) launch(inst *instance) {
	inst.mu.Lock()
	inst.launched = true
	inst.startedAt = time.Now()
	inst.mu.Unlock()
	cfg, srv, httpProxy := inst.cfg, inst.server, inst.httpProxy
	ctx := inst.ctx
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
		backoff := time.Second
		for ctx.Err() == nil {
			inst.mu.Lock()
			inst.socksRunning = true
			inst.mu.Unlock()
			err := srv.ListenAndServe(ctx)
			inst.mu.Lock()
			inst.socksRunning = false
			if err != nil {
				inst.lastError = "SOCKS5 listener: " + err.Error()
				select {
				case inst.startupErrors <- fmt.Errorf("SOCKS5 listener: %w", err):
				default:
				}
			}
			inst.mu.Unlock()
			if err == nil || ctx.Err() != nil {
				return
			}
			m.log.Error("SOCKS5 listener stopped; retrying", "server", cfg.Name, "listen", cfg.Listen, "interface", cfg.Interface, "retry_in", backoff, "error", err)
			if !waitForRetry(ctx, backoff) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	if httpProxy != nil {
		inst.wg.Add(1)
		go func() {
			defer inst.wg.Done()
			backoff := time.Second
			for ctx.Err() == nil {
				inst.mu.Lock()
				inst.httpRunning = true
				inst.mu.Unlock()
				err := httpProxy.ListenAndServe(ctx)
				inst.mu.Lock()
				inst.httpRunning = false
				if err != nil {
					inst.lastError = "HTTP proxy listener: " + err.Error()
					select {
					case inst.startupErrors <- fmt.Errorf("HTTP proxy listener: %w", err):
					default:
					}
				}
				inst.mu.Unlock()
				if err == nil || ctx.Err() != nil {
					return
				}
				m.log.Error("HTTP/HTTPS proxy listener stopped; retrying", "server", cfg.Name, "listen", cfg.HTTPProxy.Listen, "interface", cfg.Interface, "retry_in", backoff, "error", err)
				if !waitForRetry(ctx, backoff) {
					return
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		}()
	}
	go func() { defer inst.wg.Done(); m.heartbeatLoop(inst.heartbeatCtx, inst) }()
}

func (m *Manager) stop(id int64) {
	m.transition.Lock()
	defer m.transition.Unlock()
	m.mu.Lock()
	inst := m.instances[id]
	delete(m.instances, id)
	m.mu.Unlock()
	if inst == nil {
		return
	}
	m.shutdownInstance(inst)
}

func (m *Manager) shutdownInstance(inst *instance) {
	inst.heartbeatCancel()
	inst.cancel()
	_ = inst.server.Close()
	if inst.httpProxy != nil {
		_ = inst.httpProxy.Close()
	}
	inst.wg.Wait()
	m.log.Info("server instance stopped", "server", inst.cfg.Name, "interface", inst.cfg.Interface)
}

func (m *Manager) waitReady(parent context.Context, inst *instance) error {
	ctx, cancel := context.WithTimeout(parent, reloadStartupTimeout)
	defer cancel()
	socksReady := inst.server.Ready()
	var httpReady <-chan struct{}
	if inst.httpProxy != nil {
		httpReady = inst.httpProxy.Ready()
	}
	for socksReady != nil || httpReady != nil {
		select {
		case <-socksReady:
			socksReady = nil
		case <-httpReady:
			httpReady = nil
		case err := <-inst.startupErrors:
			return err
		case <-ctx.Done():
			return fmt.Errorf("listener startup: %w", ctx.Err())
		}
	}
	return nil
}

func (m *Manager) scheduleDrain(inst *instance) {
	// The retired instance must not race the replacement when persisting the
	// same server_id heartbeat while its established sessions are draining.
	inst.heartbeatCancel()
	ctx, cancel := context.WithTimeout(m.ctx, reloadDrainTimeout)
	m.drainMu.Lock()
	m.draining[inst] = cancel
	m.drainWG.Add(1)
	m.drainMu.Unlock()
	go func() {
		defer m.drainWG.Done()
		defer cancel()
		m.drainInstance(ctx, inst)
		m.drainMu.Lock()
		delete(m.draining, inst)
		m.drainMu.Unlock()
	}()
}

func (m *Manager) drainInstance(ctx context.Context, inst *instance) {
	// HTTP forwarding uses the SOCKS server's bound dial context. Drain it first
	// so a SOCKS graceful close cannot cancel DNS/connect work belonging to an
	// already accepted HTTP request. The shared deadline still caps total drain.
	if inst.httpProxy != nil {
		if err := inst.httpProxy.GracefulClose(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			m.log.Warn("HTTP proxy session drain ended with error", "server", inst.cfg.Name, "error", err)
		}
	}
	if err := inst.server.GracefulClose(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		m.log.Warn("SOCKS5 session drain ended with error", "server", inst.cfg.Name, "error", err)
	}
	inst.cancel()
	inst.wg.Wait()
	m.log.Info("previous server instance drained", "server", inst.cfg.Name, "interface", inst.cfg.Interface)
}

func (i *instance) stopAccepting() {
	i.server.StopAccepting()
	if i.httpProxy != nil {
		i.httpProxy.StopAccepting()
	}
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

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
