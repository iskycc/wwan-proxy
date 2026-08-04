package socks5

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestBindDomainRejectsWrongPeerAndContinues(t *testing.T) {
	dnsAddress, queries := startBootstrapDNS(t, [4]byte{127, 0, 0, 1})
	srv := New(config.Server{
		BindTimeout: config.Duration(2 * time.Second),
		IdleTimeout: config.Duration(2 * time.Second),
		DNS:         config.DNS{Servers: []string{dnsAddress}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

	serverControl, clientControl := tcpConnectionPair(t)
	defer serverControl.Close()
	defer clientControl.Close()
	done := make(chan error, 1)
	go func() {
		done <- srv.handleBindContext(context.Background(), serverControl, address{Host: "bind-peer.test", Port: 0})
	}()

	bound := readSuccessReply(t, clientControl)
	if queries.a.Load() == 0 {
		t.Fatal("BIND hostname was not resolved through the configured resolver")
	}

	wrong := dialBindPeerFrom(t, bound, net.IPv4(127, 0, 0, 2))
	_ = wrong.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := wrong.Read(one[:]); err == nil {
		t.Fatal("wrong BIND peer was not closed")
	}
	_ = wrong.Close()

	peer := dialBindPeerFrom(t, bound, net.IPv4(127, 0, 0, 1))
	defer peer.Close()
	second := readSuccessReply(t, clientControl)
	if !second.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("second BIND reply peer=%s, want 127.0.0.1", second.IP)
	}

	if _, err := peer.Write([]byte("from-peer")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("from-peer"))
	if _, err := io.ReadFull(clientControl, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-peer" {
		t.Fatalf("control received %q", got)
	}
	if _, err := clientControl.Write([]byte("from-client")); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len("from-client"))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-client" {
		t.Fatalf("peer received %q", got)
	}

	_ = clientControl.Close()
	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BIND relay did not stop")
	}
}

func TestBindPendingAcceptStopsOnCancelAndControlClose(t *testing.T) {
	tests := []struct {
		name string
		stop func(context.CancelFunc, net.Conn)
	}{
		{name: "context cancellation", stop: func(cancel context.CancelFunc, _ net.Conn) { cancel() }},
		{name: "control close", stop: func(_ context.CancelFunc, client net.Conn) { _ = client.Close() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := New(config.Server{BindTimeout: config.Duration(time.Minute)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			defer srv.Close()
			serverControl, clientControl := tcpConnectionPair(t)
			defer serverControl.Close()
			defer clientControl.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- srv.handleBindContext(ctx, serverControl, address{Host: "0.0.0.0", Port: 0})
			}()
			_ = readSuccessReply(t, clientControl)

			started := time.Now()
			test.stop(cancel, clientControl)
			select {
			case <-done:
				if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
					t.Fatalf("pending BIND stopped too slowly: %v", elapsed)
				}
			case <-time.After(time.Second):
				t.Fatal("pending BIND did not stop")
			}
		})
	}
}

func TestBindPeerConstraintSupportsIPv6(t *testing.T) {
	constraint := bindPeerConstraint{port: 443, ips: []net.IP{net.ParseIP("2001:db8::10")}}
	if !bindPeerAllowed(&net.TCPAddr{IP: net.ParseIP("2001:db8::10"), Port: 443}, constraint) {
		t.Fatal("matching IPv6 peer was rejected")
	}
	if bindPeerAllowed(&net.TCPAddr{IP: net.ParseIP("2001:db8::11"), Port: 443}, constraint) {
		t.Fatal("non-matching IPv6 peer was accepted")
	}
}

func TestBindPortWildcardChoosesFamilyWithAnyAllowedPort(t *testing.T) {
	srv := New(config.Server{Access: config.AccessControl{
		TargetDefault: "deny",
		TargetRules:   []string{"allow [2001:db8::/32]:443"},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	ips := []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")}
	allowed := ips[:0]
	for _, ip := range ips {
		if srv.access.AllowTargetOnAnyPort("peer.example", ip) {
			allowed = append(allowed, ip)
		}
	}
	network, err := selectBindNetwork(allowed, true, true)
	if err != nil || len(allowed) != 1 || allowed[0].To4() != nil || network != "tcp6" {
		t.Fatalf("allowed=%v network=%s err=%v, want IPv6 only", allowed, network, err)
	}
}

func TestBindExplicitIPv6AdvertiseSelectsIPv6ForDualStackDomain(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	_ = probe.Close()

	dnsAddress, queries := startDualStackDNS(t)
	srv := New(config.Server{
		BindTimeout: config.Duration(2 * time.Second),
		IdleTimeout: config.Duration(2 * time.Second),
		Bind:        config.SOCKS5Bind{Advertise: "::1"},
		DNS:         config.DNS{Servers: []string{dnsAddress}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

	serverControl, clientControl := tcpConnectionPair(t)
	defer serverControl.Close()
	defer clientControl.Close()
	done := make(chan error, 1)
	go func() {
		done <- srv.handleBindContext(context.Background(), serverControl, address{Host: "dual-stack.test", Port: 0})
	}()
	bound := readSuccessReply(t, clientControl)
	if queries.a.Load() == 0 || queries.aaaa.Load() == 0 {
		t.Fatalf("dual-stack lookup counts: A=%d AAAA=%d", queries.a.Load(), queries.aaaa.Load())
	}
	if !bound.IP.Equal(net.ParseIP("::1")) {
		t.Fatalf("BIND advertised %s, want ::1", bound.IP)
	}

	dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("::1")}}
	peer, err := dialer.Dial("tcp6", bound.String())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	second := readSuccessReply(t, clientControl)
	if !second.IP.Equal(net.ParseIP("::1")) {
		t.Fatalf("second BIND reply peer=%v, want ::1", second)
	}
	_ = clientControl.Close()
	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dual-stack BIND relay did not stop")
	}
}

func TestSelectBindNetworkUsesIPv6WhenAutoInterfaceIsIPv6Only(t *testing.T) {
	ips := []net.IP{net.IPv4(192, 0, 2, 10), net.ParseIP("2001:db8::10")}
	network, err := selectBindNetwork(ips, false, true)
	if err != nil || network != "tcp6" {
		t.Fatalf("network=%q err=%v, want tcp6", network, err)
	}
}

func TestBindWildcardAuthorizesActualPeerPort(t *testing.T) {
	allowedPort := unusedTCPPort(t)
	srv := New(config.Server{
		BindTimeout: config.Duration(2 * time.Second),
		IdleTimeout: config.Duration(2 * time.Second),
		Access: config.AccessControl{
			TargetDefault: "deny",
			TargetRules:   []string{fmt.Sprintf("allow 127.0.0.1:%d", allowedPort)},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

	serverControl, clientControl := tcpConnectionPair(t)
	defer serverControl.Close()
	defer clientControl.Close()
	done := make(chan error, 1)
	go func() {
		done <- srv.handleBindContext(context.Background(), serverControl, address{Host: "0.0.0.0", Port: 0})
	}()
	bound := readSuccessReply(t, clientControl)

	denied := dialBindPeerFromAddr(t, bound, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	_ = denied.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := denied.Read(one[:]); err == nil {
		t.Fatal("BIND peer on a denied actual port was accepted")
	}
	_ = denied.Close()

	peer := dialBindPeerFromAddr(t, bound, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: allowedPort})
	defer peer.Close()
	second := readSuccessReply(t, clientControl)
	if second.Port != allowedPort {
		t.Fatalf("second BIND reply port=%d, want %d", second.Port, allowedPort)
	}

	_ = clientControl.Close()
	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BIND relay did not stop")
	}
}

func TestBindInFlightCompletesAfterListenerHandoff(t *testing.T) {
	srv := New(config.Server{
		BindTimeout: config.Duration(2 * time.Second),
		IdleTimeout: config.Duration(2 * time.Second),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	serverControl, clientControl := tcpConnectionPair(t)
	defer serverControl.Close()
	defer clientControl.Close()
	done := make(chan error, 1)
	go func() {
		done <- srv.handleBindContext(context.Background(), serverControl, address{Host: "0.0.0.0", Port: 0})
	}()
	bound := readSuccessReply(t, clientControl)

	// Hot reload only stops new accepts. A BIND command that already returned
	// its listening address must still accept its peer and send the second reply.
	srv.StopAccepting()
	peer := dialBindPeerFrom(t, bound, net.IPv4(127, 0, 0, 1))
	defer peer.Close()
	second := readSuccessReply(t, clientControl)
	if !second.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("second BIND reply peer=%v", second)
	}
	_ = clientControl.Close()
	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight BIND did not finish after listener handoff")
	}
}

func tcpConnectionPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server := <-accepted:
		return server, client
	case err := <-acceptErr:
		_ = client.Close()
		t.Fatal(err)
		return nil, nil
	}
}

func dialBindPeerFrom(t *testing.T, bound *net.UDPAddr, sourceIP net.IP) net.Conn {
	return dialBindPeerFromAddr(t, bound, &net.TCPAddr{IP: sourceIP})
}

func dialBindPeerFromAddr(t *testing.T, bound *net.UDPAddr, source *net.TCPAddr) net.Conn {
	t.Helper()
	dialer := net.Dialer{
		Timeout:   time.Second,
		LocalAddr: source,
	}
	// readSuccessReply is shared with UDP ASSOCIATE tests and therefore returns
	// *net.UDPAddr, but its String method is also a valid TCP endpoint.
	conn, err := dialer.Dial("tcp4", bound.String())
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startDualStackDNS(t *testing.T) (string, *bootstrapDNSCounts) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var queries bootstrapDNSCounts
	go func() {
		packet := make([]byte, 4096)
		for {
			n, peer, readErr := conn.ReadFrom(packet)
			if readErr != nil {
				return
			}
			query := append([]byte(nil), packet[:n]...)
			queryType, queryErr := dnsTestQueryType(query)
			if queryErr != nil {
				continue
			}
			var response []byte
			switch queryType {
			case 1:
				queries.a.Add(1)
				response, queryErr = dnsTestResponseIPv4(query, [4]byte{127, 0, 0, 1})
			case 28:
				queries.aaaa.Add(1)
				response, queryErr = dnsTestResponseIPv6(query, net.ParseIP("::1"))
			default:
				continue
			}
			if queryErr == nil {
				_, _ = conn.WriteTo(response, peer)
			}
		}
	}()
	return conn.LocalAddr().String(), &queries
}

func dnsTestResponseIPv6(query []byte, ip net.IP) ([]byte, error) {
	queryType, questionEnd, err := dnsTestQuestion(query)
	if err != nil {
		return nil, err
	}
	response := append([]byte(nil), query[:questionEnd]...)
	binary.BigEndian.PutUint16(response[2:4], 0x8180)
	binary.BigEndian.PutUint16(response[8:10], 0)
	binary.BigEndian.PutUint16(response[10:12], 0)
	if queryType != 28 || ip.To16() == nil {
		binary.BigEndian.PutUint16(response[6:8], 0)
		return response, nil
	}
	binary.BigEndian.PutUint16(response[6:8], 1)
	response = append(response, 0xc0, 0x0c, 0, 28, 0, 1)
	response = binary.BigEndian.AppendUint32(response, 60)
	response = append(response, 0, 16)
	response = append(response, ip.To16()...)
	return response, nil
}
