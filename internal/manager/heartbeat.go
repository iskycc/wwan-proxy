package manager

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"

	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

const heartbeatURL = "https://1.1.1.1/cdn-cgi/trace"

var heartbeatFailureReminder = 5 * time.Minute

func (m *Manager) maybeTriggerVohiveRecovery(inst *instance, consecutiveFailures int) {
	if inst.vohiveClient == nil || inst.cfg.VohiveDeviceID == "" {
		return
	}
	threshold := inst.vohiveSettings.ConsecutiveFailures
	if threshold == 0 {
		threshold = 2
	}
	if consecutiveFailures < threshold {
		return
	}
	inst.mu.Lock()
	if inst.vohiveInProgress {
		inst.mu.Unlock()
		return
	}
	cooldown := time.Duration(inst.vohiveSettings.Cooldown)
	if cooldown == 0 {
		cooldown = 5 * time.Minute
	}
	if !inst.lastVohiveAttempt.IsZero() && time.Since(inst.lastVohiveAttempt) < cooldown {
		inst.mu.Unlock()
		return
	}
	inst.vohiveInProgress = true
	inst.lastVohiveAttempt = time.Now()
	inst.mu.Unlock()

	go func() {
		defer func() {
			inst.mu.Lock()
			inst.vohiveInProgress = false
			inst.mu.Unlock()
		}()
		m.vohiveRecovery(m.ctx, inst, inst.cfg.VohiveDeviceID)
	}()
}

func (m *Manager) runVohiveRecovery(ctx context.Context, inst *instance, deviceID string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	m.recordVohiveEvent(ctx, store.VohiveEventRecoveryStarted, deviceID, &inst.cfg.ID, fmt.Sprintf("recovery started for server %s", inst.cfg.Name), nil)

	var status vohive.NetworkStatus
	status, err := inst.vohiveClient.RestartDevice(ctx, deviceID)
	if err != nil {
		m.log.Error("vohive device restart failed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "error", err)
		m.recordVohiveEvent(ctx, store.VohiveEventRecoveryFailed, deviceID, &inst.cfg.ID, fmt.Sprintf("restart failed: %v", err), map[string]any{"error": err.Error()})
		return err
	}
	if !status.NetworkConnected {
		m.log.Error("vohive device restart did not restore network connectivity", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "status", status.Status)
		m.recordVohiveEvent(ctx, store.VohiveEventRecoveryFailed, deviceID, &inst.cfg.ID, "device restart did not restore network connectivity", map[string]any{"status": status.Status})
		return nil
	}

	m.log.Info("vohive device network restarted, reloading instance", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "private_ip", status.PrivateIP)
	if err := m.vohiveReload(ctx, inst.cfg.ID); err != nil {
		m.log.Error("failed to reload instance after vohive recovery", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "error", err)
		m.recordVohiveEvent(ctx, store.VohiveEventRecoveryFailed, deviceID, &inst.cfg.ID, fmt.Sprintf("reload failed: %v", err), map[string]any{"error": err.Error()})
		return err
	}

	if m.vohivePostRestartSleep != nil {
		if err := m.vohivePostRestartSleep(ctx); err != nil {
			m.log.Warn("vohive recovery aborted during post-restart wait", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "error", err)
			m.recordVohiveEvent(ctx, store.VohiveEventRecoveryFailed, deviceID, &inst.cfg.ID, fmt.Sprintf("post-restart wait aborted: %v", err), map[string]any{"error": err.Error()})
			return err
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		status, err := inst.vohiveClient.GetNetworkStatus(ctx, deviceID)
		if err != nil {
			m.log.Warn("vohive status check failed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "attempt", attempt+1, "error", err)
		} else if status.PublicIP != "" {
			m.log.Info("vohive recovery confirmed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "public_ip", status.PublicIP)
			m.recordVohiveEvent(ctx, store.VohiveEventRecoverySucceeded, deviceID, &inst.cfg.ID, fmt.Sprintf("recovery confirmed for server %s", inst.cfg.Name), map[string]any{"public_ip": status.PublicIP})
			return nil
		} else {
			m.log.Warn("vohive status check returned empty public_ip", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "attempt", attempt+1)
		}
		if attempt < 2 && m.vohiveStatusRetryDelay != nil {
			if err := m.vohiveStatusRetryDelay(ctx); err != nil {
				m.log.Warn("vohive recovery aborted during status retry wait", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID, "error", err)
				m.recordVohiveEvent(ctx, store.VohiveEventRecoveryFailed, deviceID, &inst.cfg.ID, fmt.Sprintf("status retry wait aborted: %v", err), map[string]any{"error": err.Error()})
				return err
			}
		}
	}
	m.log.Warn("vohive recovery completed but public_ip was not confirmed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", deviceID)
	m.recordVohiveEvent(ctx, store.VohiveEventRecoveryFailed, deviceID, &inst.cfg.ID, "public_ip was not confirmed after restart", nil)
	return nil
}

func (m *Manager) heartbeatCheckResult(inst *instance, probe heartbeatProbeResult) {
	h := probe.heartbeat
	endpoint := inst.cfg.Heartbeat.URL
	timeout := inst.cfg.Heartbeat.Timeout.Value(12 * time.Second)
	previousHealthy := inst.heartbeatPrevHealthy
	if !h.Healthy {
		inst.heartbeatConsecutiveFailures++
		if inst.heartbeatFirstFailure.IsZero() {
			inst.heartbeatFirstFailure = h.CheckedAt
		}
		inst.setError("heartbeat: " + h.Error)
		iface := collectInterfaceDiagnostic(inst.cfg.Interface)
		signature := probe.failureStage + "\x00" + h.Error + "\x00" + iface.signature()
		now := time.Now()
		if previousHealthy == nil || *previousHealthy || signature != inst.heartbeatPreviousFailureSignature || now.Sub(inst.heartbeatLastFailureLog) >= heartbeatFailureReminder {
			args := []any{
				"server", inst.cfg.Name,
				"interface", inst.cfg.Interface,
				"endpoint", sanitizeHeartbeatEndpoint(endpoint),
				"target_host", heartbeatTargetHost(endpoint),
				"stage", probe.failureStage,
				"classification", classifyHeartbeatFailure(probe.failureStage, probe.cause),
				"timeout", timeout,
				"latency_ms", h.LatencyMS,
				"http_status", h.StatusCode,
				"consecutive_failures", inst.heartbeatConsecutiveFailures,
				"failure_duration", now.Sub(inst.heartbeatFirstFailure).Round(time.Millisecond),
				"error", h.Error,
				"error_chain", heartbeatErrorChain(probe.cause, endpoint),
			}
			args = append(args, probe.trace.logAttrs(endpoint)...)
			args = append(args, heartbeatDNSLogAttrs(inst.cfg)...)
			args = append(args, iface.logAttrs()...)
			m.log.Error("egress heartbeat failed", args...)
			inst.heartbeatLastFailureLog = now
		}
		inst.heartbeatPreviousFailureSignature = signature
		m.maybeTriggerVohiveRecovery(inst, inst.heartbeatConsecutiveFailures)
		m.enterVohiveFastMode()
	} else if previousHealthy != nil && !*previousHealthy {
		inst.clearHeartbeatError()
		m.log.Warn("egress heartbeat recovered", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "endpoint", sanitizeHeartbeatEndpoint(endpoint), "latency_ms", h.LatencyMS, "public_ip", h.PublicIP, "colo", h.Colo, "failed_checks", inst.heartbeatConsecutiveFailures, "failure_duration", time.Since(inst.heartbeatFirstFailure).Round(time.Millisecond))
		inst.heartbeatFirstFailure = time.Time{}
		inst.heartbeatLastFailureLog = time.Time{}
		inst.heartbeatPreviousFailureSignature = ""
		inst.heartbeatConsecutiveFailures = 0
		m.leaveVohiveFastMode()
		m.maybeReloadAfterHeartbeatRecovery(inst)
	} else if previousHealthy == nil {
		m.log.Info("egress heartbeat healthy", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "endpoint", sanitizeHeartbeatEndpoint(endpoint), "latency_ms", h.LatencyMS, "public_ip", h.PublicIP, "colo", h.Colo)
	}
	healthy := h.Healthy
	inst.heartbeatPrevHealthy = &healthy
}

func (m *Manager) maybeReloadAfterHeartbeatRecovery(inst *instance) {
	if inst.cfg.VohiveDeviceID == "" || m.vohiveHealth == nil {
		return
	}
	m.vohiveHealth.mu.RLock()
	dh, ok := m.vohiveHealth.devices[inst.cfg.VohiveDeviceID]
	lastErr := m.vohiveHealth.lastError
	m.vohiveHealth.mu.RUnlock()
	if lastErr != "" {
		return
	}
	if ok && dh.Healthy {
		if err := m.vohiveReload(m.ctx, inst.cfg.ID); err != nil {
			m.log.Error("failed to reload server after heartbeat recovery", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "device", inst.cfg.VohiveDeviceID, "error", err)
		}
	}
}

func (m *Manager) heartbeatLoop(ctx context.Context, inst *instance) {
	timeout := inst.cfg.Heartbeat.Timeout.Value(12 * time.Second)
	interval := inst.cfg.Heartbeat.Interval.Value(30 * time.Second)
	endpoint := inst.cfg.Heartbeat.URL
	transport := &http.Transport{
		Proxy: nil, DialContext: inst.server.ProbeDialContext, ForceAttemptHTTP2: true,
		IdleConnTimeout: interval + 15*time.Second, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// ProbeDialContext intentionally bypasses client target ACLs. Never let a
		// heartbeat endpoint expand that privilege through an HTTP redirect.
		CheckRedirect: rejectHeartbeatRedirect,
	}
	check := func() {
		probe := executeHeartbeat(ctx, client, inst.cfg.ID, endpoint, timeout)
		if ctx.Err() != nil {
			return
		}
		if err := m.store.SaveHeartbeat(ctx, probe.heartbeat); err != nil {
			m.log.Error("save heartbeat status failed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "error", err)
		}
		m.heartbeatCheckResult(inst, probe)
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func rejectHeartbeatRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func performHeartbeat(parent context.Context, client *http.Client, serverID int64) store.Heartbeat {
	return performHeartbeatAt(parent, client, serverID, heartbeatURL)
}

func performHeartbeatAt(parent context.Context, client *http.Client, serverID int64, endpoint string) store.Heartbeat {
	return performHeartbeatWithTimeout(parent, client, serverID, endpoint, 12*time.Second)
}

func performHeartbeatWithTimeout(parent context.Context, client *http.Client, serverID int64, endpoint string, timeout time.Duration) store.Heartbeat {
	return executeHeartbeat(parent, client, serverID, endpoint, timeout).heartbeat
}

type heartbeatProbeResult struct {
	heartbeat    store.Heartbeat
	failureStage string
	cause        error
	trace        heartbeatTraceSnapshot
}

func executeHeartbeat(parent context.Context, client *http.Client, serverID int64, endpoint string, timeout time.Duration) heartbeatProbeResult {
	started := time.Now()
	h := store.Heartbeat{ServerID: serverID, CheckedAt: started}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	traceState := newHeartbeatTraceState()
	ctx = httptrace.WithClientTrace(ctx, traceState.clientTrace())
	traceState.setStage("request_build")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		req.Header.Set("User-Agent", "wwan-proxy/heartbeat")
		traceState.setStage("http_round_trip")
		resp, doErr := client.Do(req)
		err = unwrapHeartbeatURLError(doErr)
		if resp != nil {
			h.StatusCode = resp.StatusCode
			traceState.setStage("response_body")
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			if err == nil {
				err = readErr
			}
			h.Trace = string(body)
			for _, line := range strings.Split(h.Trace, "\n") {
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				switch key {
				case "ip":
					h.PublicIP = value
				case "colo":
					h.Colo = value
				}
			}
			if err == nil && resp.StatusCode != http.StatusOK {
				traceState.setStage("http_status")
				err = fmt.Errorf("unexpected HTTP status %s", resp.Status)
			}
		}
	}
	h.LatencyMS = time.Since(started).Milliseconds()
	h.Healthy = err == nil
	result := heartbeatProbeResult{heartbeat: h, trace: traceState.snapshot()}
	if err != nil {
		result.failureStage = traceState.stage()
		result.cause = err
		result.heartbeat.Error = formatHeartbeatError(result.failureStage, err, endpoint)
	}
	return result
}

type heartbeatTraceState struct {
	mu                    sync.Mutex
	currentStage          string
	dnsHost               string
	dnsAddresses          []string
	dnsError              string
	connectAttempts       []string
	connectErrors         []string
	localAddress          string
	remoteAddress         string
	connectionReused      bool
	tlsVersion            string
	tlsError              string
	requestWriteError     string
	firstResponseByteSeen bool
}

type heartbeatTraceSnapshot struct {
	DNSHost               string
	DNSAddresses          []string
	DNSError              string
	ConnectAttempts       []string
	ConnectErrors         []string
	LocalAddress          string
	RemoteAddress         string
	ConnectionReused      bool
	TLSVersion            string
	TLSError              string
	RequestWriteError     string
	FirstResponseByteSeen bool
}

func newHeartbeatTraceState() *heartbeatTraceState { return &heartbeatTraceState{} }

func (s *heartbeatTraceState) setStage(stage string) {
	s.mu.Lock()
	s.currentStage = stage
	s.mu.Unlock()
}

func (s *heartbeatTraceState) stage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentStage == "" {
		return "unknown"
	}
	return s.currentStage
}

func (s *heartbeatTraceState) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			s.mu.Lock()
			s.currentStage = "dns"
			s.dnsHost = info.Host
			s.mu.Unlock()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			s.mu.Lock()
			for _, address := range info.Addrs {
				s.dnsAddresses = appendUniqueLimited(s.dnsAddresses, address.String(), 16)
			}
			if info.Err != nil {
				// A custom resolver (notably DoH) can perform its own TCP/TLS work
				// under the same trace while the outer DNS lookup is in flight.
				// Those callbacks may temporarily advance currentStage; the final
				// DNSDone result is authoritative for heartbeat classification.
				s.currentStage = "dns"
				s.dnsError = info.Err.Error()
			} else {
				s.currentStage = "tcp_connect"
			}
			s.mu.Unlock()
		},
		ConnectStart: func(network, address string) {
			s.mu.Lock()
			s.currentStage = "tcp_connect"
			s.connectAttempts = appendUniqueLimited(s.connectAttempts, network+" "+address, 16)
			s.mu.Unlock()
		},
		ConnectDone: func(network, address string, err error) {
			s.mu.Lock()
			if err != nil {
				s.connectErrors = appendUniqueLimited(s.connectErrors, network+" "+address+": "+err.Error(), 16)
			} else {
				s.currentStage = "http_round_trip"
			}
			s.mu.Unlock()
		},
		TLSHandshakeStart: func() { s.setStage("tls_handshake") },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			s.mu.Lock()
			if err != nil {
				s.tlsError = err.Error()
			} else {
				s.tlsVersion = tlsVersionName(state.Version)
				s.currentStage = "response_headers"
			}
			s.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			s.mu.Lock()
			s.currentStage = "response_headers"
			s.connectionReused = info.Reused
			if info.Conn != nil {
				s.localAddress = info.Conn.LocalAddr().String()
				s.remoteAddress = info.Conn.RemoteAddr().String()
			}
			s.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				return
			}
			s.mu.Lock()
			s.currentStage = "request_write"
			s.requestWriteError = info.Err.Error()
			s.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			s.mu.Lock()
			s.currentStage = "response_headers"
			s.firstResponseByteSeen = true
			s.mu.Unlock()
		},
	}
}

func (s *heartbeatTraceState) snapshot() heartbeatTraceSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return heartbeatTraceSnapshot{
		DNSHost: s.dnsHost, DNSAddresses: append([]string(nil), s.dnsAddresses...), DNSError: s.dnsError,
		ConnectAttempts: append([]string(nil), s.connectAttempts...), ConnectErrors: append([]string(nil), s.connectErrors...),
		LocalAddress: s.localAddress, RemoteAddress: s.remoteAddress, ConnectionReused: s.connectionReused,
		TLSVersion: s.tlsVersion, TLSError: s.tlsError, RequestWriteError: s.requestWriteError,
		FirstResponseByteSeen: s.firstResponseByteSeen,
	}
}

func (s heartbeatTraceSnapshot) logAttrs(endpoint string) []any {
	return []any{
		"dns_host", s.DNSHost, "resolved_addresses", s.DNSAddresses, "dns_error", sanitizeHeartbeatErrorText(s.DNSError, endpoint),
		"connect_attempts", s.ConnectAttempts, "connect_errors", sanitizeHeartbeatErrorTexts(s.ConnectErrors, endpoint),
		"local_address", s.LocalAddress, "remote_address", s.RemoteAddress, "connection_reused", s.ConnectionReused,
		"tls_version", s.TLSVersion, "tls_error", sanitizeHeartbeatErrorText(s.TLSError, endpoint), "request_write_error", sanitizeHeartbeatErrorText(s.RequestWriteError, endpoint),
		"first_response_byte", s.FirstResponseByteSeen,
	}
}

func appendUniqueLimited(values []string, value string, limit int) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	if len(values) >= limit {
		return values
	}
	return append(values, value)
}

func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}

func unwrapHeartbeatURLError(err error) error {
	if requestErr, ok := err.(*url.Error); ok {
		return fmt.Errorf("HTTP %s: %w", requestErr.Op, requestErr.Err)
	}
	return err
}
