package manager

import (
	"context"
	"log/slog"
	"time"

	"wwan-proxy/internal/httpproxy"
	"wwan-proxy/internal/socks5"
	"wwan-proxy/internal/store"
)

const statsRetention = 7 * 24 * time.Hour

// counterSnapshot captures the cumulative counters for a single server at a
// specific point in time, used to compute per-minute deltas.
type counterSnapshot struct {
	startedAt   time.Time
	metrics     socks5.MetricsSnapshot
	httpMetrics httpproxy.MetricsSnapshot
}

// statsCollector records per-minute server statistics by diffing cumulative
// counter snapshots from each server and persisting the deltas.
type statsCollector struct {
	m     *Manager
	log   *slog.Logger
	last  map[int64]counterSnapshot
	prune time.Time
}

func (m *Manager) startStatsCollector() {
	ctx, cancel := context.WithCancel(m.ctx)
	m.statsCancel = cancel
	c := &statsCollector{
		m:     m,
		log:   m.log.With("component", "stats_collector"),
		last:  make(map[int64]counterSnapshot),
		prune: time.Now().UTC(),
	}
	m.statsWG.Add(1)
	go func() {
		defer m.statsWG.Done()
		c.run(ctx)
	}()
}

func (c *statsCollector) run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	c.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *statsCollector) collect(ctx context.Context) {
	heartbeats, err := c.m.store.ListHeartbeats(ctx)
	if err != nil {
		c.log.Warn("failed to load heartbeats for stats collection", "error", err)
		heartbeats = make(map[int64]store.Heartbeat)
	}

	snapshots := c.m.Snapshots()
	rows := make([]store.ServerStats, 0, len(snapshots))
	currentIDs := make(map[int64]struct{}, len(snapshots))
	now := time.Now().UTC()
	for _, snap := range snapshots {
		currentIDs[snap.ID] = struct{}{}
		cur := counterSnapshot{
			startedAt:   snap.StartedAt,
			metrics:     snap.Metrics,
			httpMetrics: snap.HTTPMetrics,
		}
		prev := c.last[snap.ID]
		if !prev.startedAt.IsZero() && !prev.startedAt.Equal(cur.startedAt) {
			prev = counterSnapshot{}
		}
		hb := heartbeats[snap.ID]
		row := computeStatsDelta(snap.ID, cur, prev, snap.Metrics.ActiveConnections, hb.Healthy, hb.LatencyMS)
		row.Bucket = now.Truncate(time.Minute)
		row.InstanceStartedAt = snap.StartedAt
		rows = append(rows, row)
		c.last[snap.ID] = cur
	}

	for id := range c.last {
		if _, ok := currentIDs[id]; !ok {
			delete(c.last, id)
		}
	}

	if err := c.m.store.SaveServerStats(ctx, rows); err != nil {
		c.log.Warn("failed to save server stats", "error", err)
	}

	if time.Since(c.prune) >= time.Hour {
		n, err := c.m.store.PruneServerStats(ctx, statsRetention)
		if err != nil {
			c.log.Warn("failed to prune server stats", "error", err)
		} else {
			c.log.Info("pruned server stats", "rows", n)
		}
		c.prune = now
	}
}

// computeStatsDelta returns a store.ServerStats row representing the delta
// between the current and previous counter snapshots for a server.
func computeStatsDelta(serverID int64, cur, prev counterSnapshot, active int64, healthy bool, latency int64) store.ServerStats {
	return store.ServerStats{
		ServerID:           serverID,
		TCPUploadBytes:     subOrCurrent(cur.metrics.TCPUploadBytes, prev.metrics.TCPUploadBytes),
		TCPDownloadBytes:   subOrCurrent(cur.metrics.TCPDownloadBytes, prev.metrics.TCPDownloadBytes),
		UDPUploadBytes:     subOrCurrent(cur.metrics.UDPUploadBytes, prev.metrics.UDPUploadBytes),
		UDPDownloadBytes:   subOrCurrent(cur.metrics.UDPDownloadBytes, prev.metrics.UDPDownloadBytes),
		HTTPUploadBytes:    subOrCurrent(cur.httpMetrics.UploadBytes, prev.httpMetrics.UploadBytes),
		HTTPDownloadBytes:  subOrCurrent(cur.httpMetrics.DownloadBytes, prev.httpMetrics.DownloadBytes),
		TotalConnections:   subOrCurrent(cur.metrics.TotalConnections, prev.metrics.TotalConnections),
		ConnectionErrors:   subOrCurrent(cur.metrics.ConnectionErrors, prev.metrics.ConnectionErrors),
		TotalRequests:      subOrCurrent(cur.httpMetrics.TotalRequests, prev.httpMetrics.TotalRequests),
		RequestErrors:      subOrCurrent(cur.httpMetrics.RequestErrors, prev.httpMetrics.RequestErrors),
		ActiveConnections:  active,
		HeartbeatLatencyMs: latency,
		HeartbeatHealthy:   healthy,
	}
}

// subOrCurrent returns cur - prev, or cur if prev is greater (e.g. due to a
// counter reset or server restart).
func subOrCurrent(cur, prev uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}
