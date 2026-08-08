package netrelay

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestBidirectionalCopiesAndCountsBothDirections(t *testing.T) {
	client, relayClient := net.Pipe()
	relayUpstream, upstream := net.Pipe()
	defer client.Close()
	defer upstream.Close()
	var upload, download atomic.Uint64
	done := make(chan error, 1)
	go func() { done <- Bidirectional(relayClient, relayUpstream, time.Minute, &upload, &download) }()

	go func() { _, _ = client.Write([]byte("upload")) }()
	got := make([]byte, len("upload"))
	if _, err := io.ReadFull(upstream, got); err != nil || string(got) != "upload" {
		t.Fatalf("upload=%q err=%v", got, err)
	}
	go func() { _, _ = upstream.Write([]byte("download")) }()
	got = make([]byte, len("download"))
	if _, err := io.ReadFull(client, got); err != nil || string(got) != "download" {
		t.Fatalf("download=%q err=%v", got, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (upload.Load() != uint64(len("upload")) || download.Load() != uint64(len("download"))) {
		time.Sleep(time.Millisecond)
	}
	if upload.Load() != uint64(len("upload")) || download.Load() != uint64(len("download")) {
		t.Fatalf("upload=%d download=%d", upload.Load(), download.Load())
	}
	_ = client.Close()
	_ = upstream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop")
	}
}

func TestBidirectionalSharedIdleWatchdogClosesTunnel(t *testing.T) {
	client, relayClient := net.Pipe()
	relayUpstream, upstream := net.Pipe()
	defer client.Close()
	defer upstream.Close()
	var upload, download atomic.Uint64
	done := make(chan error, 1)
	go func() { done <- Bidirectional(relayClient, relayUpstream, 20*time.Millisecond, &upload, &download) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle relay remained open")
	}
}
