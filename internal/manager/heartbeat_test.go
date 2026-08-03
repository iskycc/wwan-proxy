package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHeartbeatTraceAndFailureDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ip=203.0.113.8\ncolo=SJC\nwarp=off\n"))
	}))
	defer server.Close()
	h := performHeartbeatAt(context.Background(), server.Client(), 7, server.URL)
	if !h.Healthy || h.PublicIP != "203.0.113.8" || h.Colo != "SJC" || h.ServerID != 7 {
		t.Fatalf("unexpected heartbeat %+v", h)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	h = performHeartbeatAt(context.Background(), failing.Client(), 7, failing.URL)
	if h.Healthy || h.StatusCode != http.StatusServiceUnavailable || h.Error == "" {
		t.Fatalf("expected detailed failure %+v", h)
	}
}
