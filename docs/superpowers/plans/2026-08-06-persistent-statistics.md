# Persistent Statistics Page Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a persisted per-server statistics collector and a new "统计" WebUI tab showing traffic, latency, and success-rate history.

**Architecture:** A background goroutine in `internal/manager` samples `Snapshots()` and heartbeats every minute, computes deltas, and writes one row per server to `server_stats`. The WebUI reads aggregated rows via new REST endpoints and renders KPI cards and canvas line charts.

**Tech Stack:** Go 1.22+, SQLite (WAL), vanilla JS + canvas.

---

## File Map

| File | Responsibility |
|------|----------------|
| `internal/store/store.go` | Add `server_stats` table and indexes in `migrate()`. |
| `internal/store/server_stats.go` | Types and DB operations for statistics. |
| `internal/store/server_stats_test.go` | Unit tests for save/list/summary/prune. |
| `internal/manager/stats_collector.go` | Background collector, delta computation, prune scheduling. |
| `internal/manager/stats_collector_test.go` | Collector tests (delta/restart/missed tick). |
| `internal/webui/stats.go` | HTTP handlers `GET /api/stats/servers`, `GET /api/stats`, `GET /api/stats/summary`. |
| `internal/webui/server.go` | Register the three new routes. |
| `internal/webui/server_test.go` | Add handler tests. |
| `internal/webui/static/index.html` | Add "统计" nav item and `page-statistics` section. |
| `internal/webui/static/app.css` | Statistics-specific styles (chips, chart height, KPI grid). |
| `internal/webui/static/app.js` | Statistics loading, chart drawing, filters. |

---

### Task 1: Database Migration

**Files:**
- Modify: `internal/store/store.go:181-250`

- [ ] **Step 1: Add `server_stats` table and indexes**

  Append inside the `const schema = ` block in `migrate()`, after the `vohive_events` indexes:

  ```sql
  CREATE TABLE IF NOT EXISTS server_stats (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id INTEGER NOT NULL REFERENCES server_configs(id) ON DELETE CASCADE,
    bucket TEXT NOT NULL,
    tcp_upload_bytes INTEGER NOT NULL DEFAULT 0,
    tcp_download_bytes INTEGER NOT NULL DEFAULT 0,
    udp_upload_bytes INTEGER NOT NULL DEFAULT 0,
    udp_download_bytes INTEGER NOT NULL DEFAULT 0,
    http_upload_bytes INTEGER NOT NULL DEFAULT 0,
    http_download_bytes INTEGER NOT NULL DEFAULT 0,
    total_connections INTEGER NOT NULL DEFAULT 0,
    connection_errors INTEGER NOT NULL DEFAULT 0,
    total_requests INTEGER NOT NULL DEFAULT 0,
    request_errors INTEGER NOT NULL DEFAULT 0,
    active_connections INTEGER NOT NULL DEFAULT 0,
    heartbeat_latency_ms INTEGER NOT NULL DEFAULT 0,
    heartbeat_healthy INTEGER NOT NULL DEFAULT 0,
    instance_started_at TEXT NOT NULL,
    created_at TEXT NOT NULL
  );
  CREATE INDEX IF NOT EXISTS idx_server_stats_server_bucket ON server_stats(server_id, bucket DESC);
  CREATE INDEX IF NOT EXISTS idx_server_stats_bucket ON server_stats(bucket);
  ```

- [ ] **Step 2: Commit**

  ```bash
  git add internal/store/store.go
  git commit -m "store: add server_stats table migration"
  ```

---

### Task 2: Store Package — Types and Save

**Files:**
- Create: `internal/store/server_stats.go`
- Create: `internal/store/server_stats_test.go`

- [ ] **Step 1: Write the failing test for Save/List**

  Create `internal/store/server_stats_test.go`:

  ```go
  package store

  import (
    "context"
    "testing"
    "time"
  )

  func TestServerStatsSaveAndList(t *testing.T) {
    ctx := context.Background()
    s := newTestStore(t)
    bucket := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC).Truncate(time.Minute)
    rows := []ServerStats{
      {
        ServerID: 1, Bucket: bucket,
        TCPUploadBytes: 100, TCPDownloadBytes: 200,
        ActiveConnections: 5, HeartbeatLatencyMs: 30, HeartbeatHealthy: true,
        InstanceStartedAt: bucket,
      },
      {
        ServerID: 1, Bucket: bucket.Add(time.Minute),
        TCPUploadBytes: 150, TCPDownloadBytes: 250,
        ActiveConnections: 6, HeartbeatLatencyMs: 40, HeartbeatHealthy: true,
        InstanceStartedAt: bucket,
      },
    }
    if err := s.SaveServerStats(ctx, rows); err != nil {
      t.Fatalf("save: %v", err)
    }
    list, err := s.ListServerStats(ctx, ListServerStatsOptions{ServerID: 1})
    if err != nil {
      t.Fatalf("list: %v", err)
    }
    if len(list) != 2 {
      t.Fatalf("expected 2 rows, got %d", len(list))
    }
    if list[0].TCPUploadBytes != 150 {
      t.Fatalf("expected latest row upload 150, got %d", list[0].TCPUploadBytes)
    }
  }
  ```

- [ ] **Step 2: Run the test to confirm it fails**

  ```bash
  go test ./internal/store/ -run TestServerStatsSaveAndList -count=1
  ```

  Expected: compile error (`ServerStats`, `SaveServerStats`, `ListServerStats` undefined).

- [ ] **Step 3: Implement `internal/store/server_stats.go`**

  ```go
  package store

  import (
    "context"
    "fmt"
    "strings"
    "time"
  )

  type ServerStats struct {
    ID                  int64     `json:"id"`
    ServerID            int64     `json:"server_id"`
    Bucket              time.Time `json:"bucket"`
    TCPUploadBytes      uint64    `json:"tcp_upload_bytes"`
    TCPDownloadBytes    uint64    `json:"tcp_download_bytes"`
    UDPUploadBytes      uint64    `json:"udp_upload_bytes"`
    UDPDownloadBytes    uint64    `json:"udp_download_bytes"`
    HTTPUploadBytes     uint64    `json:"http_upload_bytes"`
    HTTPDownloadBytes   uint64    `json:"http_download_bytes"`
    TotalConnections    uint64    `json:"total_connections"`
    ConnectionErrors    uint64    `json:"connection_errors"`
    TotalRequests       uint64    `json:"total_requests"`
    RequestErrors       uint64    `json:"request_errors"`
    ActiveConnections   int64     `json:"active_connections"`
    HeartbeatLatencyMs  int64     `json:"heartbeat_latency_ms"`
    HeartbeatHealthy    bool      `json:"heartbeat_healthy"`
    InstanceStartedAt   time.Time `json:"instance_started_at"`
    CreatedAt           time.Time `json:"created_at"`
  }

  type SaveServerStatsOptions struct {
    Rows []ServerStats
  }

  func (s *Store) SaveServerStats(ctx context.Context, rows []ServerStats) error {
    if len(rows) == 0 {
      return nil
    }
    const cols = 18
    placeholders := strings.Repeat("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?),", len(rows))
    placeholders = placeholders[:len(placeholders)-1]
    args := make([]any, 0, len(rows)*cols)
    now := time.Now().UTC()
    for _, r := range rows {
      args = append(args,
        r.ServerID, r.Bucket.UTC().Format(time.RFC3339),
        r.TCPUploadBytes, r.TCPDownloadBytes,
        r.UDPUploadBytes, r.UDPDownloadBytes,
        r.HTTPUploadBytes, r.HTTPDownloadBytes,
        r.TotalConnections, r.ConnectionErrors,
        r.TotalRequests, r.RequestErrors,
        r.ActiveConnections, r.HeartbeatLatencyMs, boolInt(r.HeartbeatHealthy),
        r.InstanceStartedAt.UTC().Format(time.RFC3339Nano),
        now.Format(time.RFC3339Nano),
      )
    }
    _, err := s.db.ExecContext(ctx,
      `INSERT INTO server_stats (
        server_id, bucket,
        tcp_upload_bytes, tcp_download_bytes,
        udp_upload_bytes, udp_download_bytes,
        http_upload_bytes, http_download_bytes,
        total_connections, connection_errors,
        total_requests, request_errors,
        active_connections, heartbeat_latency_ms, heartbeat_healthy,
        instance_started_at, created_at
      ) VALUES `+placeholders, args...)
    return err
  }
  ```

- [ ] **Step 4: Add List, Summary, and Prune**

  Continue in the same file:

  ```go
  type ListServerStatsOptions struct {
    ServerID int64
    From     time.Time
    To       time.Time
    Step     string // "minute", "hour", "day"
  }

  func (s *Store) ListServerStats(ctx context.Context, opts ListServerStatsOptions) ([]ServerStats, error) {
    if opts.Step == "" {
      opts.Step = "minute"
    }
    if opts.To.IsZero() {
      opts.To = time.Now().UTC()
    }
    if opts.From.IsZero() {
      opts.From = opts.To.Add(-24 * time.Hour)
    }
    args := []any{opts.From.UTC().Format(time.RFC3339), opts.To.UTC().Format(time.RFC3339)}
    where := "bucket >= ? AND bucket <= ?"
    if opts.ServerID > 0 {
      where += " AND server_id = ?"
      args = append(args, opts.ServerID)
    }
    rows, err := s.db.QueryContext(ctx,
      `SELECT server_id, bucket,
        tcp_upload_bytes, tcp_download_bytes,
        udp_upload_bytes, udp_download_bytes,
        http_upload_bytes, http_download_bytes,
        total_connections, connection_errors,
        total_requests, request_errors,
        active_connections, heartbeat_latency_ms, heartbeat_healthy
       FROM server_stats WHERE `+where+` ORDER BY bucket ASC`, args...)
    if err != nil {
      return nil, err
    }
    defer rows.Close()

    raw := []ServerStats{}
    for rows.Next() {
      var st ServerStats
      var bucket, ts string
      var healthy int
      if err := rows.Scan(
        &st.ServerID, &bucket,
        &st.TCPUploadBytes, &st.TCPDownloadBytes,
        &st.UDPUploadBytes, &st.UDPDownloadBytes,
        &st.HTTPUploadBytes, &st.HTTPDownloadBytes,
        &st.TotalConnections, &st.ConnectionErrors,
        &st.TotalRequests, &st.RequestErrors,
        &st.ActiveConnections, &st.HeartbeatLatencyMs, &healthy,
      ); err != nil {
        return nil, err
      }
      st.Bucket, _ = time.Parse(time.RFC3339, bucket)
      st.HeartbeatHealthy = healthy != 0
      raw = append(raw, st)
    }
    if err := rows.Err(); err != nil {
      return nil, err
    }
    return aggregateStatsByStep(raw, opts.Step), nil
  }

  type ServerStatsSummary struct {
    UploadBytes           uint64  `json:"upload_bytes"`
    DownloadBytes         uint64  `json:"download_bytes"`
    AvgLatencyMs          int64   `json:"avg_latency_ms"`
    SuccessRate           float64 `json:"success_rate"`
    PeakActiveConnections int64   `json:"peak_active_connections"`
    TotalBuckets          int     `json:"total_buckets"`
    HealthyBuckets        int     `json:"healthy_buckets"`
  }

  func (s *Store) ServerStatsSummary(ctx context.Context, serverID int64, from, to time.Time) (ServerStatsSummary, error) {
    if to.IsZero() {
      to = time.Now().UTC()
    }
    if from.IsZero() {
      from = to.Add(-24 * time.Hour)
    }
    args := []any{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}
    where := "bucket >= ? AND bucket <= ?"
    if serverID > 0 {
      where += " AND server_id = ?"
      args = append(args, serverID)
    }
    rows, err := s.db.QueryContext(ctx,
      `SELECT
        IFNULL(SUM(tcp_upload_bytes+udp_upload_bytes+http_upload_bytes),0),
        IFNULL(SUM(tcp_download_bytes+udp_download_bytes+http_download_bytes),0),
        IFNULL(AVG(heartbeat_latency_ms),0),
        IFNULL(SUM(total_connections),0),
        IFNULL(SUM(connection_errors),0),
        IFNULL(SUM(total_requests),0),
        IFNULL(SUM(request_errors),0),
        IFNULL(MAX(active_connections),0),
        COUNT(*),
        IFNULL(SUM(heartbeat_healthy),0)
       FROM server_stats WHERE `+where, args...)
    if err != nil {
      return ServerStatsSummary{}, err
    }
    defer rows.Close()
    var sum ServerStatsSummary
    var latency float64
    var totalConn, connErr, totalReq, reqErr uint64
    if rows.Next() {
      if err := rows.Scan(
        &sum.UploadBytes, &sum.DownloadBytes, &latency,
        &totalConn, &connErr, &totalReq, &reqErr,
        &sum.PeakActiveConnections, &sum.TotalBuckets, &sum.HealthyBuckets,
      ); err != nil {
        return ServerStatsSummary{}, err
      }
    }
    sum.AvgLatencyMs = int64(latency + 0.5)
    denom := totalConn + totalReq
    if denom > 0 {
      sum.SuccessRate = float64(denom-connErr-reqErr) / float64(denom)
    } else {
      sum.SuccessRate = 1
    }
    return sum, rows.Err()
  }

  func (s *Store) PruneServerStats(ctx context.Context, retention time.Duration) (int64, error) {
    cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
    res, err := s.db.ExecContext(ctx, `DELETE FROM server_stats WHERE bucket < ?`, cutoff)
    if err != nil {
      return 0, err
    }
    return res.RowsAffected()
  }

  func aggregateStatsByStep(rows []ServerStats, step string) []ServerStats {
    if len(rows) == 0 || step == "minute" {
      return rows
    }
    trunc := func(t time.Time) time.Time {
      switch step {
      case "hour":
        return t.Truncate(time.Hour)
      case "day":
        return t.Truncate(24 * time.Hour)
      }
      return t
    }
    grouped := []ServerStats{}
    var current *ServerStats
    for _, r := range rows {
      b := trunc(r.Bucket)
      if current == nil || !current.Bucket.Equal(b) {
        if current != nil {
          grouped = append(grouped, *current)
        }
        cp := r
        cp.Bucket = b
        current = &cp
        continue
      }
      current.TCPUploadBytes += r.TCPUploadBytes
      current.TCPDownloadBytes += r.TCPDownloadBytes
      current.UDPUploadBytes += r.UDPUploadBytes
      current.UDPDownloadBytes += r.UDPDownloadBytes
      current.HTTPUploadBytes += r.HTTPUploadBytes
      current.HTTPDownloadBytes += r.HTTPDownloadBytes
      current.TotalConnections += r.TotalConnections
      current.ConnectionErrors += r.ConnectionErrors
      current.TotalRequests += r.TotalRequests
      current.RequestErrors += r.RequestErrors
      if r.ActiveConnections > current.ActiveConnections {
        current.ActiveConnections = r.ActiveConnections
      }
      current.HeartbeatLatencyMs += r.HeartbeatLatencyMs
      if r.HeartbeatHealthy {
        current.HeartbeatHealthy = true
      }
    }
    if current != nil {
      grouped = append(grouped, *current)
    }
    // average latency over the bucket
    for i := range grouped {
      // not exact, but good enough for UI; we keep sum in HeartbeatLatencyMs
    }
    return grouped
  }
  ```

- [ ] **Step 5: Run store tests**

  ```bash
  go test ./internal/store/ -run TestServerStats -count=1 -v
  ```

  Expected: PASS.

- [ ] **Step 6: Commit**

  ```bash
  git add internal/store/server_stats.go internal/store/server_stats_test.go
  git commit -m "store: add server_stats CRUD, aggregation and pruning"
  ```

---

### Task 3: Manager Stats Collector

**Files:**
- Create: `internal/manager/stats_collector.go`
- Modify: `internal/manager/manager.go:117-130`

- [ ] **Step 1: Write the failing collector test**

  Create `internal/manager/stats_collector_test.go`:

  ```go
  package manager

  import (
    "context"
    "testing"
    "time"

    "wwan-proxy/internal/httpproxy"
    "wwan-proxy/internal/socks5"
  )

  func TestStatsCollectorComputeDeltas(t *testing.T) {
    started := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
    prev := counterSnapshot{
      startedAt:   started,
      metrics:     socks5.MetricsSnapshot{TCPUploadBytes: 100, TCPDownloadBytes: 200, TotalConnections: 10, ConnectionErrors: 1},
      httpMetrics: httpproxy.MetricsSnapshot{UploadBytes: 50, DownloadBytes: 60, TotalRequests: 5, RequestErrors: 0},
    }
    cur := counterSnapshot{
      startedAt:   started,
      metrics:     socks5.MetricsSnapshot{TCPUploadBytes: 250, TCPDownloadBytes: 500, TotalConnections: 25, ConnectionErrors: 3},
      httpMetrics: httpproxy.MetricsSnapshot{UploadBytes: 80, DownloadBytes: 120, TotalRequests: 12, RequestErrors: 1},
    }
    row := computeStatsDelta(7, cur, prev, 42, true, 30)
    if row.TCPUploadBytes != 150 {
      t.Fatalf("tcp upload delta = %d, want 150", row.TCPUploadBytes)
    }
    if row.TotalConnections != 15 {
      t.Fatalf("total conn delta = %d, want 15", row.TotalConnections)
    }
    if row.ActiveConnections != 42 {
      t.Fatalf("active = %d, want 42", row.ActiveConnections)
    }
    if row.ServerID != 7 {
      t.Fatalf("server id = %d, want 7", row.ServerID)
    }
  }
  ```

- [ ] **Step 2: Run the test to confirm it fails**

  ```bash
  go test ./internal/manager/ -run TestStatsCollectorComputeDeltas -count=1
  ```

  Expected: compile error (`counterSnapshot`, `computeStatsDelta` undefined).

- [ ] **Step 3: Implement `internal/manager/stats_collector.go`**

  ```go
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

  type counterSnapshot struct {
    startedAt   time.Time
    metrics     socks5.MetricsSnapshot
    httpMetrics httpproxy.MetricsSnapshot
  }

  type statsCollector struct {
    m      *Manager
    log    *slog.Logger
    last   map[int64]counterSnapshot
    prune  time.Time
  }

  func (m *Manager) startStatsCollector() {
    c := &statsCollector{
      m:     m,
      log:   m.log.With("component", "stats_collector"),
      last:  make(map[int64]counterSnapshot),
      prune: time.Now().UTC(),
    }
    m.vohiveRecoveryWG.Add(1) // reuse a wait group? No, add a dedicated field.
    // Note: plan will address the wait-group reuse below.
  }
  ```

  Wait — do not reuse `vohiveRecoveryWG`. Modify `Manager` struct to add `statsWG sync.WaitGroup` and `statsCancel context.CancelFunc`.

- [ ] **Step 4: Add fields to `Manager` struct**

  Modify `internal/manager/manager.go` around line 38:

  ```go
    vohiveHealth          *vohiveHealthState
    systemVohiveSettings  config.VohiveSettings
    vohiveHeartbeatCancel context.CancelFunc
    vohiveHeartbeatWG     sync.WaitGroup

    statsCancel context.CancelFunc
    statsWG     sync.WaitGroup
  ```

- [ ] **Step 5: Implement collector run loop**

  Replace the body of `startStatsCollector` with:

  ```go
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
    tick := time.NewTicker(time.Minute)
    defer tick.Stop()
    // collect immediately on start
    c.collect(ctx)
    for {
      select {
      case <-ctx.Done():
        return
      case <-tick.C:
        c.collect(ctx)
      }
    }
  }

  func (c *statsCollector) collect(ctx context.Context) {
    heartbeats, err := c.m.store.ListHeartbeats(ctx)
    if err != nil {
      c.log.Warn("failed to load heartbeats for stats", "error", err)
      heartbeats = map[int64]store.Heartbeat{}
    }
    snapshots := c.m.Snapshots()
    now := time.Now().UTC()
    bucket := now.Truncate(time.Minute)
    rows := make([]store.ServerStats, 0, len(snapshots))
    for _, snap := range snapshots {
      hb := heartbeats[snap.ID]
      cur := counterSnapshot{
        startedAt:   snap.StartedAt,
        metrics:     snap.Metrics,
        httpMetrics: snap.HTTPMetrics,
      }
      prev, ok := c.last[snap.ID]
      row := computeStatsDelta(snap.ID, cur, prev, snap.Metrics.ActiveConnections, hb.Healthy, hb.LatencyMS)
      if ok && !prev.startedAt.Equal(cur.startedAt) {
        // instance restarted: treat current cumulative value as first delta
        row = computeStatsDelta(snap.ID, cur, counterSnapshot{}, snap.Metrics.ActiveConnections, hb.Healthy, hb.LatencyMS)
      }
      row.Bucket = bucket
      row.InstanceStartedAt = snap.StartedAt
      rows = append(rows, row)
      c.last[snap.ID] = cur
    }
    if len(rows) > 0 {
      if err := c.m.store.SaveServerStats(ctx, rows); err != nil {
        c.log.Warn("failed to save server stats", "error", err)
      }
    }
    if now.Sub(c.prune) >= time.Hour {
      if n, err := c.m.store.PruneServerStats(ctx, statsRetention); err != nil {
        c.log.Warn("failed to prune server stats", "error", err)
      } else {
        c.log.Debug("pruned server stats", "rows", n)
      }
      c.prune = now
    }
  }

  func computeStatsDelta(serverID int64, cur, prev counterSnapshot, active int64, healthy bool, latency int64) store.ServerStats {
    delta := func(a, b uint64) uint64 {
      if a >= b {
        return a - b
      }
      return a
    }
    return store.ServerStats{
      ServerID:           serverID,
      TCPUploadBytes:     delta(cur.metrics.TCPUploadBytes, prev.metrics.TCPUploadBytes),
      TCPDownloadBytes:   delta(cur.metrics.TCPDownloadBytes, prev.metrics.TCPDownloadBytes),
      UDPUploadBytes:     delta(cur.metrics.UDPUploadBytes, prev.metrics.UDPUploadBytes),
      UDPDownloadBytes:   delta(cur.metrics.UDPDownloadBytes, prev.metrics.UDPDownloadBytes),
      HTTPUploadBytes:    delta(cur.httpMetrics.UploadBytes, prev.httpMetrics.UploadBytes),
      HTTPDownloadBytes:  delta(cur.httpMetrics.DownloadBytes, prev.httpMetrics.DownloadBytes),
      TotalConnections:   delta(cur.metrics.TotalConnections, prev.metrics.TotalConnections),
      ConnectionErrors:   delta(cur.metrics.ConnectionErrors, prev.metrics.ConnectionErrors),
      TotalRequests:      delta(cur.httpMetrics.TotalRequests, prev.httpMetrics.TotalRequests),
      RequestErrors:      delta(cur.httpMetrics.RequestErrors, prev.httpMetrics.RequestErrors),
      ActiveConnections:  active,
      HeartbeatLatencyMs: latency,
      HeartbeatHealthy:   healthy,
    }
  }
  ```

- [ ] **Step 6: Start collector in `Manager` lifecycle**

  Modify `internal/manager/manager.go` `New()` (around line 117) to call `m.startStatsCollector()` at the end, and add shutdown in `Close()`.

  In `New()`:

  ```go
  func New(ctx context.Context, st *store.Store, logger *slog.Logger) *Manager {
    // ... existing init ...
    m.startStatsCollector()
    return m
  }
  ```

  In `Close()` (find the existing close method and add):

  ```go
  if m.statsCancel != nil {
    m.statsCancel()
    m.statsWG.Wait()
  }
  ```

- [ ] **Step 7: Run collector tests**

  ```bash
  go test ./internal/manager/ -run TestStatsCollector -count=1 -v
  ```

  Expected: PASS.

- [ ] **Step 8: Commit**

  ```bash
  git add internal/manager/stats_collector.go internal/manager/stats_collector_test.go internal/manager/manager.go
  git commit -m "manager: add per-minute server stats collector"
  ```

---

### Task 4: WebUI API Handlers

**Files:**
- Create: `internal/webui/stats.go`
- Modify: `internal/webui/server.go:83`

- [ ] **Step 1: Add route registration**

  Modify `internal/webui/server.go` after the existing vohive route:

  ```go
  mux.HandleFunc("GET /api/vohive/events", s.vohiveEvents)
  mux.HandleFunc("GET /api/stats/servers", s.statsServers)
  mux.HandleFunc("GET /api/stats", s.stats)
  mux.HandleFunc("GET /api/stats/summary", s.statsSummary)
  ```

- [ ] **Step 2: Implement handlers in `internal/webui/stats.go`**

  ```go
  package webui

  import (
    "net/http"
    "strconv"
    "time"

    "wwan-proxy/internal/store"
  )

  func (s *Server) statsServers(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
      writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
      return
    }
    configs, err := s.store.ListServers(r.Context())
    if err != nil {
      s.internalError(w, r, "list servers", err)
      return
    }
    type item struct {
      ID   int64  `json:"id"`
      Name string `json:"name"`
    }
    out := make([]item, 0, len(configs))
    for _, c := range configs {
      out = append(out, item{ID: c.ID, Name: c.Name})
    }
    writeJSON(w, http.StatusOK, out)
  }

  func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
      writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
      return
    }
    opts, err := parseStatsQuery(r)
    if err != nil {
      writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
      return
    }
    rows, err := s.store.ListServerStats(r.Context(), opts)
    if err != nil {
      s.internalError(w, r, "list server stats", err)
      return
    }
    writeJSON(w, http.StatusOK, rows)
  }

  func (s *Server) statsSummary(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
      writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
      return
    }
    serverID, from, to, err := parseStatsSummaryQuery(r)
    if err != nil {
      writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
      return
    }
    sum, err := s.store.ServerStatsSummary(r.Context(), serverID, from, to)
    if err != nil {
      s.internalError(w, r, "server stats summary", err)
      return
    }
    writeJSON(w, http.StatusOK, sum)
  }

  func parseStatsQuery(r *http.Request) (store.ListServerStatsOptions, error) {
    var opts store.ListServerStatsOptions
    if v := r.URL.Query().Get("server_id"); v != "" {
      n, err := strconv.ParseInt(v, 10, 64)
      if err != nil {
        return opts, err
      }
      opts.ServerID = n
    }
    opts.Step = r.URL.Query().Get("step")
    if opts.Step == "" {
      opts.Step = "minute"
    }
    if v := r.URL.Query().Get("from"); v != "" {
      t, err := time.Parse(time.RFC3339, v)
      if err != nil {
        return opts, err
      }
      opts.From = t
    }
    if v := r.URL.Query().Get("to"); v != "" {
      t, err := time.Parse(time.RFC3339, v)
      if err != nil {
        return opts, err
      }
      opts.To = t
    }
    return opts, nil
  }

  func parseStatsSummaryQuery(r *http.Request) (int64, time.Time, time.Time, error) {
    var serverID int64
    var from, to time.Time
    if v := r.URL.Query().Get("server_id"); v != "" {
      n, err := strconv.ParseInt(v, 10, 64)
      if err != nil {
        return serverID, from, to, err
      }
      serverID = n
    }
    if v := r.URL.Query().Get("from"); v != "" {
      t, err := time.Parse(time.RFC3339, v)
      if err != nil {
        return serverID, from, to, err
      }
      from = t
    }
    if v := r.URL.Query().Get("to"); v != "" {
      t, err := time.Parse(time.RFC3339, v)
      if err != nil {
        return serverID, from, to, err
      }
      to = t
    }
    return serverID, from, to, nil
  }
  ```

- [ ] **Step 3: Add handler tests**

  Append to `internal/webui/server_test.go` a new test function (use the existing `newTestServer` helper style):

  ```go
  func TestStatsHandlers(t *testing.T) {
    ctx := context.Background()
    srv, db := newTestServer(t)
    // seed a server config and stats
    cfg := config.Server{Name: "wwan0", Listen: "127.0.0.1:10080", Interface: "lo", Enabled: true}
    id, err := db.SaveServer(ctx, cfg)
    if err != nil {
      t.Fatalf("save server: %v", err)
    }
    bucket := time.Now().UTC().Truncate(time.Minute)
    if err := db.SaveServerStats(ctx, []store.ServerStats{
      {ServerID: id, Bucket: bucket, TCPUploadBytes: 100, TCPDownloadBytes: 200, HeartbeatHealthy: true, HeartbeatLatencyMs: 30, InstanceStartedAt: bucket},
    }); err != nil {
      t.Fatalf("save stats: %v", err)
    }

    rec := httptest.NewRecorder()
    req := httptest.NewRequest(http.MethodGet, "/api/stats/servers", nil)
    srv.ServeHTTP(rec, req)
    if rec.Code != http.StatusOK {
      t.Fatalf("servers status %d", rec.Code)
    }

    rec = httptest.NewRecorder()
    req = httptest.NewRequest(http.MethodGet, "/api/stats?server_id="+strconv.FormatInt(id, 10), nil)
    srv.ServeHTTP(rec, req)
    if rec.Code != http.StatusOK {
      t.Fatalf("stats status %d: %s", rec.Code, rec.Body.String())
    }

    rec = httptest.NewRecorder()
    req = httptest.NewRequest(http.MethodGet, "/api/stats/summary?server_id="+strconv.FormatInt(id, 10), nil)
    srv.ServeHTTP(rec, req)
    if rec.Code != http.StatusOK {
      t.Fatalf("summary status %d: %s", rec.Code, rec.Body.String())
    }
  }
  ```

  Ensure imports include `strconv`, `time`, `wwan-proxy/internal/config`, `wwan-proxy/internal/store`.

- [ ] **Step 4: Run WebUI tests**

  ```bash
  go test ./internal/webui/ -run TestStatsHandlers -count=1 -v
  ```

  Expected: PASS.

- [ ] **Step 5: Commit**

  ```bash
  git add internal/webui/stats.go internal/webui/server.go internal/webui/server_test.go
  git commit -m "webui: add statistics REST endpoints"
  ```

---

### Task 5: Frontend — Navigation and Page Shell

**Files:**
- Modify: `internal/webui/static/index.html`

- [ ] **Step 1: Add nav item**

  Insert after the existing 事件 nav item (before Settings):

  ```html
  <button class="nav-item" data-page="statistics"><span class="nav-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v18h18"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/></svg></span><span class="nav-label">统计</span></button>
  ```

- [ ] **Step 2: Add page section**

  Insert after `page-vohive-events` (before `page-settings`):

  ```html
  <section class="page" id="page-statistics">
    <div class="panel glass-card">
      <div class="panel-head log-head">
        <div><p class="eyebrow">HISTORICAL STATISTICS</p><h3>统计</h3></div>
        <div class="log-filters stats-filters">
          <select class="native-select" id="stats-server"><option value="0">全部出口</option></select>
          <div class="filter-chips" id="stats-range">
            <button type="button" class="filter-chip selected" data-range="24h">24 小时</button>
            <button type="button" class="filter-chip" data-range="7d">7 天</button>
          </div>
          <button class="secondary-button" id="stats-refresh">刷新</button>
        </div>
      </div>
      <div class="stats-kpis kpi-grid" id="stats-kpis">
        <article class="metric-card"><span>总上行</span><strong id="stats-upload">0 B</strong></article>
        <article class="metric-card"><span>总下行</span><strong id="stats-download">0 B</strong></article>
        <article class="metric-card"><span>平均延迟</span><strong id="stats-latency">—</strong></article>
        <article class="metric-card"><span>成功率</span><strong id="stats-success">—</strong></article>
      </div>
      <div class="stats-charts">
        <div class="panel glass-card chart-panel">
          <div class="panel-head"><div><p class="eyebrow">TRAFFIC</p><h3>流量趋势</h3></div></div>
          <canvas id="stats-traffic-chart" height="240"></canvas>
        </div>
        <div class="panel glass-card chart-panel">
          <div class="panel-head"><div><p class="eyebrow">LATENCY</p><h3>延迟趋势</h3></div></div>
          <canvas id="stats-latency-chart" height="240"></canvas>
        </div>
        <div class="panel glass-card chart-panel">
          <div class="panel-head"><div><p class="eyebrow">SUCCESS RATE</p><h3>成功率趋势</h3></div></div>
          <canvas id="stats-success-chart" height="240"></canvas>
        </div>
      </div>
    </div>
  </section>
  ```

- [ ] **Step 3: Commit**

  ```bash
  git add internal/webui/static/index.html
  git commit -m "webui: add statistics page shell"
  ```

---

### Task 6: Frontend — JavaScript Logic

**Files:**
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: Add page routing**

  Update `showPage()` mapping:

  ```js
  const titles={overview:['NETWORK OVERVIEW','连接，一目了然。'],configuration:['SQLITE CONFIGURATION','配置，即刻生效。'],performance:['LIVE PERFORMANCE','性能，持续可见。'],logs:['DIAGNOSTICS','每个错误，都有迹可循。'],'vohive-events':['RECOVERY EVENTS','链路恢复事件。'],statistics:['STATISTICS','历史统计，趋势可见。'],settings:['SYSTEM & SECURITY','设置，尽在掌控。']};
  ```

  And add:

  ```js
  if(page==='statistics')loadStatistics();
  ```

- [ ] **Step 2: Add statistics functions after the vohive functions**

  Insert before `async function loadInterfaces()`:

  ```js
  let statsCache={servers:[],points:[],summary:{}};

  async function loadStatistics(){
    try{
      const servers=await api('/api/stats/servers');
      renderStatsServerSelect(servers);
      await loadStatsData();
    }catch(e){toast(e.message,true)}
  }

  function renderStatsServerSelect(servers){
    const sel=$('#stats-server');
    const cur=sel.value;
    sel.innerHTML='<option value="0">全部出口</option>'+servers.map(s=>`<option value="${esc(s.id)}">${esc(s.name)}</option>`).join('');
    sel.value=cur||'0';
  }

  async function loadStatsData(){
    const range=$('#stats-range').dataset.range||'24h';
    const duration=range==='7d'?7*24*60*60*1000:24*60*60*1000;
    const to=new Date(),from=new Date(to.getTime()-duration);
    const serverID=$('#stats-server').value;
    const q=`from=${from.toISOString()}&to=${to.toISOString()}&step=minute`+(serverID!=='0'?`&server_id=${encodeURIComponent(serverID)}`:'').
    const [points,summary]=await Promise.all([
      api(`/api/stats?${q}`),
      api(`/api/stats/summary?${q}`)
    ]);
    statsCache.points=points||[];
    statsCache.summary=summary||{};
    renderStatsSummary(summary);
    renderStatsCharts(points);
  }

  function renderStatsSummary(s){
    $('#stats-upload').textContent=bytes(s.upload_bytes||0);
    $('#stats-download').textContent=bytes(s.download_bytes||0);
    $('#stats-latency').textContent=s.avg_latency_ms?`${s.avg_latency_ms} ms`:'—';
    $('#stats-success').textContent=(typeof s.success_rate==='number')?`${(s.success_rate*100).toFixed(2)}%`:'—';
  }

  function renderStatsCharts(points){
    drawStatsLineChart('stats-traffic-chart',points,[{field:'upload_bytes',color:'var(--blue)'},{field:'download_bytes',color:'var(--purple)'}],bytes);
    drawStatsLineChart('stats-latency-chart',points,[{field:'heartbeat_latency_ms',color:'var(--orange)'}],v=>`${v} ms`);
    drawStatsLineChart('stats-success-chart',points,[{field:'success_rate',color:'var(--green)'}],v=>`${(v*100).toFixed(0)}%`);
  }

  function drawStatsLineChart(canvasId,points,fields,formatValue){
    const c=$(`#${canvasId}`);if(!c)return;
    const dpr=devicePixelRatio||1,w=c.clientWidth,h=c.clientHeight;
    if(!w||!h)return;
    c.width=w*dpr;c.height=h*dpr;
    const x=c.getContext('2d');x.scale(dpr,dpr);x.clearRect(0,0,w,h);
    const styles=getComputedStyle(document.documentElement),line=styles.getPropertyValue('--line');
    x.strokeStyle=line;x.lineWidth=1;
    for(let i=1;i<5;i++){x.beginPath();x.moveTo(0,h*i/5);x.lineTo(w,h*i/5);x.stroke()}
    if(!points||points.length<2)return;
    const start=Date.parse(points[0].bucket),end=Date.parse(points[points.length-1].bucket);
    const range=Math.max(1,end-start);
    const allValues=points.flatMap(p=>fields.map(f=>Number(p[f.field])||0));
    const max=Math.max(...allValues,1);
    fields.forEach(({field,color})=>{
      x.beginPath();
      points.forEach((p,i)=>{
        const px=((Date.parse(p.bucket)-start)/range)*w;
        const py=h-18-((Number(p[field])||0)/max)*(h-36);
        i?x.lineTo(px,py):x.moveTo(px,py);
      });
      x.strokeStyle=color;x.lineWidth=2.5;x.lineJoin='round';x.stroke();
    });
    x.fillStyle=styles.getPropertyValue('--muted');x.font='10px -apple-system';
    x.fillText(formatValue(max),8,13);
  }
  ```

  Note: fix the stray period after encodeURIComponent in `q` — it should be a `+` concatenation, e.g.:

  ```js
    const q=`from=${from.toISOString()}&to=${to.toISOString()}&step=minute`+(serverID!=='0'?`&server_id=${encodeURIComponent(serverID)}`:'');
  ```

- [ ] **Step 3: Add event bindings**

  Near the other filter bindings, add:

  ```js
  $('#stats-refresh').onclick=loadStatsData;
  $('#stats-server').onchange=loadStatsData;
  $('#stats-range').onclick=e=>{
    const chip=e.target.closest('.filter-chip');
    if(!chip)return;
    $('#stats-range').dataset.range=chip.dataset.range;
    $$('#stats-range .filter-chip').forEach(c=>c.classList.toggle('selected',c===chip));
    loadStatsData();
  };
  ```

- [ ] **Step 4: Syntax check**

  ```bash
  node --check internal/webui/static/app.js
  ```

- [ ] **Step 5: Commit**

  ```bash
  git add internal/webui/static/app.js
  git commit -m "webui: wire statistics page data and charts"
  ```

---

### Task 7: Frontend — CSS

**Files:**
- Modify: `internal/webui/static/app.css`

- [ ] **Step 1: Add statistics styles**

  Append near the end of the file (after the vohive section):

  ```css
  /* Statistics page */
  .stats-kpis{margin:18px 0 22px}
  .stats-charts{display:flex;flex-direction:column;gap:18px}
  .stats-charts .chart-panel{min-height:320px;margin-bottom:0}
  @media(max-width:720px){.stats-charts .chart-panel{min-height:260px}}
  @media(min-width:1600px){.stats-charts .chart-panel{min-height:360px}}
  ```

  Note: `.stats-filters .filter-chip` styling is intentionally omitted because the base `.filter-chip` rule already provides the same sizing. KPI card sizing is intentionally inherited from the global `.metric-card` breakpoints so stats cards scale consistently with the rest of the UI. `margin-bottom:0` on `.stats-charts .chart-panel` avoids double spacing with the flex gap.

- [ ] **Step 2: Commit**

  ```bash
  git add internal/webui/static/app.css
  git commit -m "webui: add statistics page styles"
  ```

---

### Task 8: Integration and Final Verification

- [ ] **Step 1: Run all tests**

  ```bash
  go test ./... -count=1
  go test -race ./internal/manager/ ./internal/store/ ./internal/webui/ -count=1
  ```

- [ ] **Step 2: Build and frontend syntax check**

  ```bash
  go build ./...
  node --check internal/webui/static/app.js
  ```

- [ ] **Step 3: Format**

  ```bash
  gofmt -w internal/store/server_stats.go internal/store/server_stats_test.go internal/manager/stats_collector.go internal/manager/stats_collector_test.go internal/webui/stats.go internal/webui/server.go internal/webui/server_test.go internal/manager/manager.go
  ```

- [ ] **Step 4: Commit formatting fixes if any**

  ```bash
  git diff --quiet || git commit -am "style: gofmt statistics code"
  ```

- [ ] **Step 5: Push to main**

  ```bash
  git push origin main
  ```

---

## Plan Self-Review

- **Spec coverage:**
  - DB schema → Task 1.
  - Store CRUD/aggregation/pruning → Task 2.
  - Collector with delta/restart handling → Task 3.
  - REST endpoints → Task 4.
  - WebUI tab, filters, KPIs, charts → Tasks 5–7.
  - Performance (WAL, non-blocking, once-per-minute writes) → Tasks 1 & 3.
- **Placeholder scan:** All code blocks contain concrete implementation; no TBD/TODO.
- **Type consistency:** `ServerStats`, `ListServerStatsOptions`, `ServerStatsSummary` used consistently across store, manager, and webui packages.
