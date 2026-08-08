package socks5

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func startTestUpstream(t *testing.T, users map[string]string) (string, func()) {
	t.Helper()
	cfg := config.Server{
		Name: "upstream", Listen: "127.0.0.1:0", Interface: "lo",
		Auth: config.Auth{Method: "none"},
		UDP:  config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(3 * time.Second), MaxAssociations: 16},
	}
	if len(users) > 0 {
		cfg.Auth.Method = "username_password"
		cfg.Auth.Users = users
	}
	srv := New(cfg, testLogger(t))
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.wg.Add(1)
			go func() {
				defer srv.wg.Done()
				defer conn.Close()
				_ = srv.handle(conn)
			}()
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		srv.Close()
		<-done
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDialViaUpstreamNoAuth(t *testing.T) {
	addr, cleanup := startTestUpstream(t, nil)
	defer cleanup()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "none"}

	conn, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", "127.0.0.1:9999")
	if err == nil {
		_ = conn.Close()
	}
	// The upstream server has nothing on 9999, so the SOCKS5 handshake succeeds
	// but the upstream-side connect will fail with connection refused.
	if err == nil {
		t.Fatal("expected upstream-side connect to fail")
	}
}

func TestDialViaUpstreamUsernamePassword(t *testing.T) {
	addr, cleanup := startTestUpstream(t, map[string]string{"alice": "secret"})
	defer cleanup()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "username_password", Username: "alice", Password: "secret"}

	conn, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", "127.0.0.1:9999")
	if err == nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected upstream-side connect to fail")
	}
}

func TestDialViaUpstreamAuthFailure(t *testing.T) {
	addr, cleanup := startTestUpstream(t, map[string]string{"alice": "secret"})
	defer cleanup()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "username_password", Username: "alice", Password: "wrong"}

	_, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", "127.0.0.1:9999")
	if err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestDialViaUpstreamSuccess(t *testing.T) {
	echo := listenTCPEcho(t)
	defer echo.Close()
	addr, cleanup := startTestUpstream(t, nil)
	defer cleanup()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "none"}

	conn, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("dial via upstream: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
}

func TestUDPAssociateViaUpstream(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, readErr := echo.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], from)
		}
	}()

	upstreamAddress, cleanup := startTestUpstream(t, nil)
	defer cleanup()
	downstream := New(config.Server{
		Name: "downstream", Interface: "lo", ConnectTimeout: config.Duration(2 * time.Second),
		Auth:     config.Auth{Method: "none"},
		UDP:      config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(3 * time.Second), MaxAssociations: 16},
		Upstream: config.Upstream{Enabled: true, Address: upstreamAddress, AuthMethod: "none"},
	}, testLogger(t))
	control, done := runHandler(t, downstream)
	defer control.Close()
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, 0)
	relay := readSuccessReply(t, control)

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := []byte("chained-wifi-calling")
	if _, err := client.WriteToUDP(udpTestPacket(echo.LocalAddr().(*net.UDPAddr), payload), relay); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	from, got, err := parseUDPDatagram(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if from.Port != uint16(echo.LocalAddr().(*net.UDPAddr).Port) || string(got) != string(payload) {
		t.Fatalf("response from=%v payload=%q", from, got)
	}
	_ = control.Close()
	select {
	case err := <-done:
		if err != nil && !isNormalClose(err) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chained UDP association did not stop")
	}
}

func TestOpenUDPViaAuthenticatedUpstream(t *testing.T) {
	upstreamAddress, cleanup := startTestUpstream(t, map[string]string{"alice": "secret"})
	defer cleanup()
	relay, err := OpenUDPViaUpstream(context.Background(), config.Upstream{
		Enabled: true, Address: upstreamAddress, AuthMethod: "username_password", Username: "alice", Password: "secret",
	}, &net.Dialer{Timeout: 2 * time.Second}, net.DefaultResolver)
	if err != nil {
		t.Fatal(err)
	}
	if relay.Control == nil || relay.Packet == nil || relay.Relay == nil || relay.Relay.Port == 0 {
		t.Fatalf("incomplete upstream UDP relay: %+v", relay)
	}
	if err := relay.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestDialViaUpstreamTargetAddressValidation(t *testing.T) {
	upstream := config.Upstream{Enabled: true, Address: "127.0.0.1:1080", AuthMethod: "none"}
	_, err := DialViaUpstream(context.Background(), upstream, &net.Dialer{Timeout: time.Second}, "tcp", "not-an-address")
	if err == nil {
		t.Fatal("expected invalid target address error")
	}
	_, err = DialViaUpstream(context.Background(), upstream, &net.Dialer{Timeout: time.Second}, "tcp", "127.0.0.1:0")
	if err == nil {
		t.Fatal("expected invalid target port error")
	}
}

func TestDialViaUpstreamDisabled(t *testing.T) {
	upstream := config.Upstream{Enabled: false}
	_, err := DialViaUpstream(context.Background(), upstream, &net.Dialer{Timeout: time.Second}, "tcp", "127.0.0.1:80")
	if err == nil || err.Error() != "upstream is not enabled" {
		t.Fatalf("unexpected error: %v", err)
	}
}
