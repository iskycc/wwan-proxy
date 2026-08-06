# Persistent Statistics Page Design

## Goal
Add a "统计" (Statistics) tab to the WebUI that shows per-server historical metrics: traffic (upload/download), latency, and success/failure rates. Data is persisted in SQLite so it survives page reloads and process restarts, while keeping collection overhead low.

## Constraints & Decisions
- **Sampling granularity:** 1 minute.
- **Retention:** 7 days, automatic pruning.
- **Aggregation dimension:** per server (SOCKS5/HTTP proxy instance).
- **Location:** new tab, existing Performance page stays unchanged.
- **Performance requirement:** collection must not block proxy traffic or heartbeat logic.

## Data Model

### Table: `server_stats`
One row per server per minute bucket.

| Column | Type | Notes |
|--------|------|-------|
| `id` | INTEGER PK AUTOINCREMENT | |
| `server_id` | INTEGER NOT NULL | FK to `server_configs(id)` ON DELETE CASCADE |
| `bucket` | TEXT NOT NULL | Minute bucket in RFC3339 format, e.g. `2026-08-06T10:23:00Z` |
| `tcp_upload_bytes` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `tcp_download_bytes` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `udp_upload_bytes` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `udp_download_bytes` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `http_upload_bytes` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `http_download_bytes` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `total_connections` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `connection_errors` | INTEGER NOT NULL DEFAULT 0 | Delta since previous bucket |
| `total_requests` | INTEGER NOT NULL DEFAULT 0 | HTTP request delta |
| `request_errors` | INTEGER NOT NULL DEFAULT 0 | HTTP request error delta |
| `active_connections` | INTEGER NOT NULL DEFAULT 0 | Snapshot value at sample time |
| `heartbeat_latency_ms` | INTEGER NOT NULL DEFAULT 0 | Snapshot from latest heartbeat |
| `heartbeat_healthy` | INTEGER NOT NULL DEFAULT 0 | 0/1 snapshot |
| `instance_started_at` | TEXT NOT NULL | Server instance `started_at` used to detect restart |
| `created_at` | TEXT NOT NULL | Insertion timestamp |

Indexes:
- `idx_server_stats_server_bucket` on `(server_id, bucket DESC)`
- `idx_server_stats_bucket` on `(bucket)` for pruning

## Collection Architecture

A new `statsCollector` runs inside `internal/manager`:

1. Started in `Manager` constructor or first `Reload`.
2. Ticks every minute using `time.Ticker`.
3. On each tick:
   - Calls `m.Snapshots()` to get current per-instance metrics.
   - Calls `s.store.ListHeartbeatStatus(ctx)` to get latest latency/health per server.
   - Maintains an in-memory map `lastCounters[serverID]counters` with `startedAt` generation.
   - Computes deltas against the previous sample.
   - If `startedAt` changed (instance restarted), the first delta equals the current cumulative value (treating the new counter base as zero).
   - Inserts one `server_stats` row per server via `s.store.SaveServerStats(ctx, rows)`.
4. Runs a prune every hour, deleting buckets older than 7 days.
5. Errors are logged with `slog.Warn`/`Error`; collection never blocks proxy paths.

SQLite is already opened with `_journal_mode=WAL`, so readers (WebUI) do not block the collector's writes.

## API

### `GET /api/stats/servers`
Returns the list of servers that have statistics rows, plus all configured servers for selection.

Response:
```json
[
  {"id": 1, "name": "wwan0"},
  {"id": 2, "name": "eth1"}
]
```

### `GET /api/stats?server_id=1&from=2026-08-05T10:00:00Z&to=2026-08-06T10:00:00Z&step=minute`
Returns time-series points. Defaults:
- `from` = now - 24h
- `to` = now
- `step` = `minute` (also supports `hour`, `day`)

When `server_id` is omitted, sums across all servers.

Response point fields:
```json
{
  "bucket": "2026-08-06T10:23:00Z",
  "upload_bytes": 123456,
  "download_bytes": 789012,
  "total_connections": 42,
  "connection_errors": 1,
  "total_requests": 120,
  "request_errors": 0,
  "active_connections": 5,
  "heartbeat_latency_ms": 45,
  "heartbeat_healthy": 1
}
```

`upload_bytes` / `download_bytes` are the sum of TCP + UDP + HTTP deltas for that bucket.

### `GET /api/stats/summary?server_id=1&from=...&to=...`
Returns aggregated summary for the selected range:

```json
{
  "upload_bytes": 1073741824,
  "download_bytes": 2147483648,
  "avg_latency_ms": 38,
  "success_rate": 0.9876,
  "peak_active_connections": 23,
  "total_buckets": 1440,
  "healthy_buckets": 1400
}
```

`success_rate` = `(total_connections + total_requests - connection_errors - request_errors) / (total_connections + total_requests)`.

## WebUI

New "统计" nav tab and `page-statistics` section.

Layout:
1. **Header filters:** server selector (custom select), time-range chips (`24h`, `7d`, `custom`).
2. **KPI cards:** 总上行 / 总下行 / 平均延迟 / 成功率 / 峰值活跃连接.
3. **Charts:**
   - Traffic over time (upload + download lines).
   - Latency over time.
   - Success rate over time.
4. **Summary table:** per-server totals for the selected range.

All components reuse existing `.panel`, `.metric-card`, `.kpi-grid`, `.table-wrap`, and the canvas chart helper pattern from `app.js`.

## Error Handling
- Collector write failures are logged; the next tick retries.
- Missing samples produce gaps in charts (no interpolation).
- API returns empty arrays and zero summaries when no data exists.

## Testing
- `internal/store/server_stats_test.go`: CRUD, pruning, aggregation queries.
- `internal/manager/stats_collector_test.go`: delta computation, restart detection, batch insert.
- `internal/webui/server_test.go`: API endpoints with query parameters.
- `node --check internal/webui/static/app.js` for frontend syntax.

## Migration
Add the `server_stats` table and indexes in `internal/store/store.go` `migrate()`.
