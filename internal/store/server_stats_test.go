package store

import (
	"context"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStore(t)
}

func TestServerStatsSaveAndList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	defer s.Close()

	cfg := testServer("stats", "127.0.0.1:11092")
	if err := s.SaveServer(ctx, &cfg); err != nil {
		t.Fatalf("save server: %v", err)
	}

	bucket := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	rows := []ServerStats{
		{
			ServerID:           cfg.ID,
			Bucket:             bucket,
			TCPUploadBytes:     100,
			TCPDownloadBytes:   200,
			ActiveConnections:  5,
			HeartbeatLatencyMs: 30,
			HeartbeatHealthy:   true,
			InstanceStartedAt:  bucket,
			CreatedAt:          bucket,
		},
		{
			ServerID:           cfg.ID,
			Bucket:             bucket.Add(time.Minute),
			TCPUploadBytes:     150,
			TCPDownloadBytes:   250,
			ActiveConnections:  6,
			HeartbeatLatencyMs: 40,
			HeartbeatHealthy:   true,
			InstanceStartedAt:  bucket,
			CreatedAt:          bucket.Add(time.Minute),
		},
	}
	if err := s.SaveServerStats(ctx, rows); err != nil {
		t.Fatalf("save: %v", err)
	}

	list, err := s.ListServerStats(ctx, ListServerStatsOptions{ServerID: cfg.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}
	if list[0].TCPUploadBytes != 100 {
		t.Fatalf("expected first row upload 100, got %d", list[0].TCPUploadBytes)
	}
	if list[1].TCPUploadBytes != 150 {
		t.Fatalf("expected second row upload 150, got %d", list[1].TCPUploadBytes)
	}

	hourList, err := s.ListServerStats(ctx, ListServerStatsOptions{
		ServerID: cfg.ID,
		From:     bucket.Add(-time.Minute),
		To:       bucket.Add(2 * time.Minute),
		Step:     "hour",
	})
	if err != nil {
		t.Fatalf("hour list: %v", err)
	}
	if len(hourList) != 1 {
		t.Fatalf("expected 1 hour row, got %d", len(hourList))
	}
	agg := hourList[0]
	if agg.TCPUploadBytes != 250 {
		t.Fatalf("expected hour upload 250, got %d", agg.TCPUploadBytes)
	}
	if agg.TCPDownloadBytes != 450 {
		t.Fatalf("expected hour download 450, got %d", agg.TCPDownloadBytes)
	}
	if agg.ActiveConnections != 6 {
		t.Fatalf("expected max active 6, got %d", agg.ActiveConnections)
	}
	if !agg.HeartbeatHealthy {
		t.Fatal("expected healthy OR true")
	}
	if agg.HeartbeatLatencyMs != 35 {
		t.Fatalf("expected avg latency 35, got %d", agg.HeartbeatLatencyMs)
	}
}

func TestServerStatsInvalidStep(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	defer s.Close()

	_, err := s.ListServerStats(ctx, ListServerStatsOptions{Step: "invalid"})
	if err == nil {
		t.Fatal("expected error for invalid step")
	}
}

func TestServerStatsSummaryAndPrune(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	defer s.Close()

	cfg := testServer("stats-summary", "127.0.0.1:11093")
	if err := s.SaveServer(ctx, &cfg); err != nil {
		t.Fatalf("save server: %v", err)
	}

	bucket := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	rows := []ServerStats{
		{
			ServerID:           cfg.ID,
			Bucket:             bucket,
			TCPUploadBytes:     100,
			TCPDownloadBytes:   200,
			ActiveConnections:  5,
			HeartbeatLatencyMs: 30,
			HeartbeatHealthy:   true,
			InstanceStartedAt:  bucket,
			CreatedAt:          bucket,
		},
		{
			ServerID:           cfg.ID,
			Bucket:             bucket.Add(time.Minute),
			TCPUploadBytes:     150,
			TCPDownloadBytes:   250,
			ActiveConnections:  6,
			HeartbeatLatencyMs: 40,
			HeartbeatHealthy:   true,
			InstanceStartedAt:  bucket,
			CreatedAt:          bucket.Add(time.Minute),
		},
	}
	if err := s.SaveServerStats(ctx, rows); err != nil {
		t.Fatalf("save: %v", err)
	}

	sum, err := s.ServerStatsSummary(ctx, cfg.ID, bucket.Add(-time.Minute), bucket.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.UploadBytes != 250 {
		t.Fatalf("expected upload 250, got %d", sum.UploadBytes)
	}
	if sum.DownloadBytes != 450 {
		t.Fatalf("expected download 450, got %d", sum.DownloadBytes)
	}
	if sum.AvgLatencyMs != 35 {
		t.Fatalf("expected avg latency 35, got %d", sum.AvgLatencyMs)
	}
	if sum.PeakActiveConnections != 6 {
		t.Fatalf("expected peak active 6, got %d", sum.PeakActiveConnections)
	}
	if sum.TotalBuckets != 2 {
		t.Fatalf("expected total buckets 2, got %d", sum.TotalBuckets)
	}
	if sum.HealthyBuckets != 2 {
		t.Fatalf("expected healthy buckets 2, got %d", sum.HealthyBuckets)
	}
	if sum.SuccessRate != 1 {
		t.Fatalf("expected success rate 1, got %f", sum.SuccessRate)
	}

	n, err := s.PruneServerStats(ctx, 30*time.Minute)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 pruned rows, got %d", n)
	}

	list, err := s.ListServerStats(ctx, ListServerStatsOptions{ServerID: cfg.ID})
	if err != nil {
		t.Fatalf("list after prune: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 rows after prune, got %d", len(list))
	}
}
