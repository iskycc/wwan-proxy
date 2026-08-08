package selfupdate

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestClientProtocol(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "update.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan agentRequest, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var request agentRequest
			_ = json.NewDecoder(conn).Decode(&request)
			requests <- request
			_ = json.NewEncoder(conn).Encode(agentResponse{OK: true})
			_ = conn.Close()
		}
	}()

	client := Client{SocketPath: socketPath, Timeout: time.Second}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Trigger(context.Background(), "lo"); err != nil {
		t.Fatal(err)
	}
	if first, second := <-requests, <-requests; first.Action != "ping" || second.Action != "update-latest" || second.Interface != "lo" {
		t.Fatalf("unexpected requests %+v, %+v", first, second)
	}
}

func TestClientMapsBusyResponse(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "update.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var request agentRequest
		_ = json.NewDecoder(conn).Decode(&request)
		_ = json.NewEncoder(conn).Encode(agentResponse{Error: "update already in progress"})
	}()
	client := Client{SocketPath: socketPath, Timeout: time.Second}
	if err := client.Trigger(context.Background(), ""); err != ErrUpdateInProgress {
		t.Fatalf("error=%v, want ErrUpdateInProgress", err)
	}
}
