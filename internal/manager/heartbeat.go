package manager

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wwan-proxy/internal/store"
)

const heartbeatURL = "https://1.1.1.1/cdn-cgi/trace"

func (m *Manager) heartbeatLoop(ctx context.Context, inst *instance) {
	timeout := inst.cfg.Heartbeat.Timeout.Value(12 * time.Second)
	interval := inst.cfg.Heartbeat.Interval.Value(30 * time.Second)
	endpoint := inst.cfg.Heartbeat.URL
	transport := &http.Transport{
		Proxy: nil, DialContext: inst.server.DialContext, ForceAttemptHTTP2: true,
		IdleConnTimeout: interval + 15*time.Second, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	var previousHealthy *bool
	check := func() {
		h := performHeartbeatWithTimeout(ctx, client, inst.cfg.ID, endpoint, timeout)
		if ctx.Err() != nil {
			return
		}
		if err := m.store.SaveHeartbeat(context.Background(), h); err != nil {
			m.log.Error("save heartbeat status failed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "error", err)
		}
		if !h.Healthy {
			inst.setError("heartbeat: " + h.Error)
			m.log.Error("WWAN heartbeat failed", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "url", endpoint, "latency_ms", h.LatencyMS, "error", h.Error)
		} else if previousHealthy == nil || !*previousHealthy {
			inst.clearHeartbeatError()
			m.log.Info("WWAN heartbeat healthy", "server", inst.cfg.Name, "interface", inst.cfg.Interface, "latency_ms", h.LatencyMS, "public_ip", h.PublicIP, "colo", h.Colo)
		}
		healthy := h.Healthy
		previousHealthy = &healthy
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

func performHeartbeat(parent context.Context, client *http.Client, serverID int64) store.Heartbeat {
	return performHeartbeatAt(parent, client, serverID, heartbeatURL)
}

func performHeartbeatAt(parent context.Context, client *http.Client, serverID int64, endpoint string) store.Heartbeat {
	return performHeartbeatWithTimeout(parent, client, serverID, endpoint, 12*time.Second)
}

func performHeartbeatWithTimeout(parent context.Context, client *http.Client, serverID int64, endpoint string, timeout time.Duration) store.Heartbeat {
	started := time.Now()
	h := store.Heartbeat{ServerID: serverID, CheckedAt: started}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		req.Header.Set("User-Agent", "wwan-proxy/heartbeat")
		resp, doErr := client.Do(req)
		err = doErr
		if resp != nil {
			h.StatusCode = resp.StatusCode
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
				err = fmt.Errorf("unexpected HTTP status %s", resp.Status)
			}
		}
	}
	h.LatencyMS = time.Since(started).Milliseconds()
	h.Healthy = err == nil
	if err != nil {
		h.Error = err.Error()
	}
	return h
}
