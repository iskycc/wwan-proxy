package manager

import (
	"testing"
	"time"

	"wwan-proxy/internal/httpproxy"
	"wwan-proxy/internal/socks5"
)

func TestStatsCollectorComputeDeltas(t *testing.T) {
	cur := counterSnapshot{
		startedAt: time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC),
		metrics: socks5.MetricsSnapshot{
			TCPUploadBytes:   1000,
			TCPDownloadBytes: 2000,
			UDPUploadBytes:   300,
			UDPDownloadBytes: 400,
			TotalConnections: 50,
			ConnectionErrors: 5,
		},
		httpMetrics: httpproxy.MetricsSnapshot{
			UploadBytes:   500,
			DownloadBytes: 600,
			TotalRequests: 30,
			RequestErrors: 3,
		},
	}
	prev := counterSnapshot{
		startedAt: time.Date(2026, 8, 6, 7, 0, 0, 0, time.UTC),
		metrics: socks5.MetricsSnapshot{
			TCPUploadBytes:   400,
			TCPDownloadBytes: 800,
			UDPUploadBytes:   100,
			UDPDownloadBytes: 150,
			TotalConnections: 20,
			ConnectionErrors: 2,
		},
		httpMetrics: httpproxy.MetricsSnapshot{
			UploadBytes:   200,
			DownloadBytes: 250,
			TotalRequests: 10,
			RequestErrors: 1,
		},
	}

	row := computeStatsDelta(7, cur, prev, 12, true, 42)
	if row.ServerID != 7 {
		t.Fatalf("ServerID=%d, want 7", row.ServerID)
	}
	want := map[string]uint64{
		"TCPUploadBytes":    600,
		"TCPDownloadBytes":  1200,
		"UDPUploadBytes":    200,
		"UDPDownloadBytes":  250,
		"HTTPUploadBytes":   300,
		"HTTPDownloadBytes": 350,
		"TotalConnections":  30,
		"ConnectionErrors":  3,
		"TotalRequests":     20,
		"RequestErrors":     2,
	}
	got := map[string]uint64{
		"TCPUploadBytes":    row.TCPUploadBytes,
		"TCPDownloadBytes":  row.TCPDownloadBytes,
		"UDPUploadBytes":    row.UDPUploadBytes,
		"UDPDownloadBytes":  row.UDPDownloadBytes,
		"HTTPUploadBytes":   row.HTTPUploadBytes,
		"HTTPDownloadBytes": row.HTTPDownloadBytes,
		"TotalConnections":  row.TotalConnections,
		"ConnectionErrors":  row.ConnectionErrors,
		"TotalRequests":     row.TotalRequests,
		"RequestErrors":     row.RequestErrors,
	}
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("%s=%d, want %d", name, g, w)
		}
	}
	if row.ActiveConnections != 12 {
		t.Errorf("ActiveConnections=%d, want 12", row.ActiveConnections)
	}
	if !row.HeartbeatHealthy {
		t.Error("HeartbeatHealthy=false, want true")
	}
	if row.HeartbeatLatencyMs != 42 {
		t.Errorf("HeartbeatLatencyMs=%d, want 42", row.HeartbeatLatencyMs)
	}
}

func TestStatsCollectorComputeDeltasZeroPrev(t *testing.T) {
	cur := counterSnapshot{
		startedAt: time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC),
		metrics: socks5.MetricsSnapshot{
			TCPUploadBytes:   1000,
			TCPDownloadBytes: 2000,
			TotalConnections: 50,
			ConnectionErrors: 5,
		},
		httpMetrics: httpproxy.MetricsSnapshot{
			UploadBytes:   500,
			DownloadBytes: 600,
			TotalRequests: 30,
			RequestErrors: 3,
		},
	}

	row := computeStatsDelta(7, cur, counterSnapshot{}, 0, false, 0)
	if row.TCPUploadBytes != 1000 {
		t.Errorf("TCPUploadBytes=%d, want 1000", row.TCPUploadBytes)
	}
	if row.TCPDownloadBytes != 2000 {
		t.Errorf("TCPDownloadBytes=%d, want 2000", row.TCPDownloadBytes)
	}
	if row.TotalConnections != 50 {
		t.Errorf("TotalConnections=%d, want 50", row.TotalConnections)
	}
	if row.ActiveConnections != 0 {
		t.Errorf("ActiveConnections=%d, want 0", row.ActiveConnections)
	}
	if row.HeartbeatHealthy {
		t.Error("HeartbeatHealthy=true, want false")
	}
}

func TestStatsCollectorRestartDetection(t *testing.T) {
	cur := counterSnapshot{
		startedAt: time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC),
		metrics: socks5.MetricsSnapshot{
			TCPUploadBytes:   100,
			TCPDownloadBytes: 200,
			TotalConnections: 5,
			ConnectionErrors: 1,
		},
		httpMetrics: httpproxy.MetricsSnapshot{
			UploadBytes:   50,
			DownloadBytes: 60,
			TotalRequests: 3,
			RequestErrors: 0,
		},
	}

	// The collector detects a restart by comparing startedAt values and passes a
	// zero prev snapshot to computeStatsDelta, making the delta equal the current
	// counters.
	row := computeStatsDelta(7, cur, counterSnapshot{}, 1, true, 10)
	if row.TCPUploadBytes != 100 {
		t.Errorf("restart delta TCPUploadBytes=%d, want 100", row.TCPUploadBytes)
	}
	if row.TotalConnections != 5 {
		t.Errorf("restart delta TotalConnections=%d, want 5", row.TotalConnections)
	}
	if row.TotalRequests != 3 {
		t.Errorf("restart delta TotalRequests=%d, want 3", row.TotalRequests)
	}
	if row.ActiveConnections != 1 {
		t.Errorf("restart delta ActiveConnections=%d, want 1", row.ActiveConnections)
	}
	if row.HeartbeatLatencyMs != 10 {
		t.Errorf("restart delta HeartbeatLatencyMs=%d, want 10", row.HeartbeatLatencyMs)
	}
}

func TestStatsCollectorUnderflowGuard(t *testing.T) {
	cur := counterSnapshot{
		startedAt: time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC),
		metrics: socks5.MetricsSnapshot{
			TCPUploadBytes: 100,
		},
		httpMetrics: httpproxy.MetricsSnapshot{
			TotalRequests: 5,
		},
	}
	prev := counterSnapshot{
		startedAt: time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC),
		metrics: socks5.MetricsSnapshot{
			TCPUploadBytes: 500,
		},
		httpMetrics: httpproxy.MetricsSnapshot{
			TotalRequests: 10,
		},
	}

	row := computeStatsDelta(7, cur, prev, 0, false, 0)
	if row.TCPUploadBytes != 100 {
		t.Errorf("underflow TCPUploadBytes=%d, want 100", row.TCPUploadBytes)
	}
	if row.TotalRequests != 5 {
		t.Errorf("underflow TotalRequests=%d, want 5", row.TotalRequests)
	}
}
