package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ServerStats stores per-server, per-bucket traffic and health metrics.
type ServerStats struct {
	ID                 int64
	ServerID           int64
	Bucket             time.Time
	TCPUploadBytes     uint64
	TCPDownloadBytes   uint64
	UDPUploadBytes     uint64
	UDPDownloadBytes   uint64
	HTTPUploadBytes    uint64
	HTTPDownloadBytes  uint64
	TotalConnections   uint64
	ConnectionErrors   uint64
	TotalRequests      uint64
	RequestErrors      uint64
	ActiveConnections  int64
	HeartbeatLatencyMs int64
	HeartbeatHealthy   bool
	InstanceStartedAt  time.Time
	CreatedAt          time.Time
}

// ListServerStatsOptions controls which server_stats rows are returned and how
// they are aggregated.
type ListServerStatsOptions struct {
	ServerID int64
	From     time.Time
	To       time.Time
	Step     string // "minute", "hour", or "day"
}

// SaveServerStats inserts all rows in a single statement. It ignores any
// caller-provided CreatedAt and sets created_at to the current UTC time.
func (s *Store) SaveServerStats(ctx context.Context, rows []ServerStats) error {
	if len(rows) == 0 {
		return nil
	}

	now := time.Now().UTC()
	placeholders := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*17)
	for _, r := range rows {
		placeholders = append(placeholders, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args,
			r.ServerID,
			r.Bucket.UTC().Format(time.RFC3339),
			r.TCPUploadBytes,
			r.TCPDownloadBytes,
			r.UDPUploadBytes,
			r.UDPDownloadBytes,
			r.HTTPUploadBytes,
			r.HTTPDownloadBytes,
			r.TotalConnections,
			r.ConnectionErrors,
			r.TotalRequests,
			r.RequestErrors,
			r.ActiveConnections,
			r.HeartbeatLatencyMs,
			boolInt(r.HeartbeatHealthy),
			r.InstanceStartedAt.UTC().Format(time.RFC3339Nano),
			now.Format(time.RFC3339Nano),
		)
	}

	stmt := `INSERT INTO server_stats(
		server_id, bucket,
		tcp_upload_bytes, tcp_download_bytes,
		udp_upload_bytes, udp_download_bytes,
		http_upload_bytes, http_download_bytes,
		total_connections, connection_errors,
		total_requests, request_errors,
		active_connections, heartbeat_latency_ms, heartbeat_healthy,
		instance_started_at, created_at
	) VALUES ` + strings.Join(placeholders, ",")

	_, err := s.db.ExecContext(ctx, stmt, args...)
	return err
}

// ListServerStats returns rows in the requested time range, optionally filtered
// by server_id, ordered by bucket ascending. When Step is "hour" or "day",
// per-minute rows are aggregated in Go.
func (s *Store) ListServerStats(ctx context.Context, opts ListServerStatsOptions) ([]ServerStats, error) {
	if opts.Step == "" {
		opts.Step = "minute"
	}
	switch opts.Step {
	case "minute", "hour", "day":
		// ok
	default:
		return nil, fmt.Errorf("unsupported step: %q", opts.Step)
	}
	now := time.Now().UTC()
	if opts.To.IsZero() {
		opts.To = now
	}
	if opts.From.IsZero() {
		opts.From = opts.To.Add(-24 * time.Hour)
	}

	fromStr := opts.From.UTC().Format(time.RFC3339)
	toStr := opts.To.UTC().Format(time.RFC3339)
	args := []any{fromStr, toStr}
	stmt := `SELECT id, server_id, bucket,
		tcp_upload_bytes, tcp_download_bytes,
		udp_upload_bytes, udp_download_bytes,
		http_upload_bytes, http_download_bytes,
		total_connections, connection_errors,
		total_requests, request_errors,
		active_connections, heartbeat_latency_ms, heartbeat_healthy,
		instance_started_at, created_at
	FROM server_stats
	WHERE bucket >= ? AND bucket <= ?`
	if opts.ServerID != 0 {
		stmt += " AND server_id = ?"
		args = append(args, opts.ServerID)
	}
	stmt += " ORDER BY bucket ASC"

	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var raw []ServerStats
	for rows.Next() {
		var st ServerStats
		var bucket, started, created string
		var healthy int
		if err := rows.Scan(
			&st.ID, &st.ServerID, &bucket,
			&st.TCPUploadBytes, &st.TCPDownloadBytes,
			&st.UDPUploadBytes, &st.UDPDownloadBytes,
			&st.HTTPUploadBytes, &st.HTTPDownloadBytes,
			&st.TotalConnections, &st.ConnectionErrors,
			&st.TotalRequests, &st.RequestErrors,
			&st.ActiveConnections, &st.HeartbeatLatencyMs, &healthy,
			&started, &created,
		); err != nil {
			return nil, err
		}
		st.Bucket, err = time.Parse(time.RFC3339, bucket)
		if err != nil {
			return nil, err
		}
		st.InstanceStartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		st.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		st.HeartbeatHealthy = healthy != 0
		raw = append(raw, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if opts.Step == "minute" {
		return raw, nil
	}
	return aggregateStatsByStep(raw, opts.Step), nil
}

func aggregateStatsByStep(raw []ServerStats, step string) []ServerStats {
	if len(raw) == 0 {
		return nil
	}

	var trunc func(time.Time) time.Time
	switch step {
	case "hour":
		trunc = func(t time.Time) time.Time { return t.Truncate(time.Hour) }
	case "day":
		trunc = func(t time.Time) time.Time { return t.Truncate(24 * time.Hour) }
	default:
		return raw
	}

	type agg struct {
		ServerStats
		count int
	}

	var out []ServerStats
	var cur *agg
	for _, r := range raw {
		tb := trunc(r.Bucket)
		if cur == nil || !cur.Bucket.Equal(tb) {
			if cur != nil {
				cur.HeartbeatLatencyMs /= int64(cur.count)
				out = append(out, cur.ServerStats)
			}
			cur = &agg{ServerStats: r, count: 1}
			cur.Bucket = tb
			continue
		}

		cur.TCPUploadBytes += r.TCPUploadBytes
		cur.TCPDownloadBytes += r.TCPDownloadBytes
		cur.UDPUploadBytes += r.UDPUploadBytes
		cur.UDPDownloadBytes += r.UDPDownloadBytes
		cur.HTTPUploadBytes += r.HTTPUploadBytes
		cur.HTTPDownloadBytes += r.HTTPDownloadBytes
		cur.TotalConnections += r.TotalConnections
		cur.ConnectionErrors += r.ConnectionErrors
		cur.TotalRequests += r.TotalRequests
		cur.RequestErrors += r.RequestErrors
		if r.ActiveConnections > cur.ActiveConnections {
			cur.ActiveConnections = r.ActiveConnections
		}
		cur.HeartbeatHealthy = cur.HeartbeatHealthy || r.HeartbeatHealthy
		cur.HeartbeatLatencyMs += r.HeartbeatLatencyMs
		cur.count++
	}
	if cur != nil {
		cur.HeartbeatLatencyMs /= int64(cur.count)
		out = append(out, cur.ServerStats)
	}
	return out
}

// ServerStatsSummary is a rolled-up view of server statistics over a range.
type ServerStatsSummary struct {
	UploadBytes           uint64  `json:"upload_bytes"`
	DownloadBytes         uint64  `json:"download_bytes"`
	AvgLatencyMs          int64   `json:"avg_latency_ms"`
	SuccessRate           float64 `json:"success_rate"`
	PeakActiveConnections int64   `json:"peak_active_connections"`
	TotalBuckets          int     `json:"total_buckets"`
	HealthyBuckets        int     `json:"healthy_buckets"`
}

// ServerStatsSummary returns aggregated statistics for a server over a time
// range. When from or to are zero, the last 24 hours are used.
func (s *Store) ServerStatsSummary(ctx context.Context, serverID int64, from, to time.Time) (ServerStatsSummary, error) {
	var sum ServerStatsSummary
	now := time.Now().UTC()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}

	fromStr := from.UTC().Format(time.RFC3339)
	toStr := to.UTC().Format(time.RFC3339)

	args := []any{fromStr, toStr}
	where := `WHERE bucket >= ? AND bucket <= ?`
	if serverID != 0 {
		where += ` AND server_id = ?`
		args = append(args, serverID)
	}

	var totalConn, totalReq, connErr, reqErr uint64
	var healthyBuckets int
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(tcp_upload_bytes + udp_upload_bytes + http_upload_bytes), 0),
			COALESCE(SUM(tcp_download_bytes + udp_download_bytes + http_download_bytes), 0),
			COALESCE(SUM(total_connections), 0),
			COALESCE(SUM(total_requests), 0),
			COALESCE(SUM(connection_errors), 0),
			COALESCE(SUM(request_errors), 0),
			COALESCE(CAST(AVG(heartbeat_latency_ms) AS INTEGER), 0),
			COALESCE(MAX(active_connections), 0),
			COUNT(*),
			COALESCE(SUM(heartbeat_healthy), 0)
		FROM server_stats `+where, args...,
	).Scan(
		&sum.UploadBytes, &sum.DownloadBytes,
		&totalConn, &totalReq, &connErr, &reqErr,
		&sum.AvgLatencyMs, &sum.PeakActiveConnections,
		&sum.TotalBuckets, &healthyBuckets,
	)
	if err != nil {
		return sum, err
	}
	sum.HealthyBuckets = healthyBuckets

	denom := totalConn + totalReq
	if denom == 0 {
		sum.SuccessRate = 1
	} else {
		numer := int64(totalConn + totalReq - connErr - reqErr)
		if numer < 0 {
			numer = 0
		}
		sum.SuccessRate = float64(numer) / float64(denom)
	}
	return sum, nil
}

// PruneServerStats deletes buckets older than the given retention duration and
// returns the number of rows removed.
func (s *Store) PruneServerStats(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM server_stats WHERE bucket < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
