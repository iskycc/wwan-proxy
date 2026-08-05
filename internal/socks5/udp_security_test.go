package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestFixedUDPRelayPortIsExclusiveAndReleasedWithControl(t *testing.T) {
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()

	srv := newUDPTestServer(config.UDP{
		Enabled: true, BindIP: "127.0.0.1", Advertise: "auto",
		RelayPort: port, IdleTimeout: config.Duration(5 * time.Second),
	})
	first, firstDone := runHandler(t, srv)
	greet(t, first)
	request(t, first, cmdUDPAssociate, net.IPv4zero, 0)
	if relay := readSuccessReply(t, first); relay.Port != port {
		t.Fatalf("relay port=%d, want fixed port %d", relay.Port, port)
	}

	second, secondDone := runHandler(t, srv)
	greet(t, second)
	request(t, second, cmdUDPAssociate, net.IPv4zero, 0)
	rep, _ := readUDPTestReply(t, second)
	if rep == repSuccess {
		t.Fatal("second association unexpectedly acquired occupied fixed relay port")
	}
	_ = second.Close()
	select {
	case err := <-secondDone:
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("fixed-port conflict error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("conflicting association did not stop")
	}

	_ = first.Close()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("association did not stop when its TCP control connection closed")
	}

	third, thirdDone := runHandler(t, srv)
	greet(t, third)
	request(t, third, cmdUDPAssociate, net.IPv4zero, 0)
	rep, relay := readUDPTestReply(t, third)
	if rep != repSuccess {
		select {
		case handlerErr := <-thirdDone:
			t.Fatalf("reacquiring fixed relay port returned reply=%d: %v", rep, handlerErr)
		case <-time.After(time.Second):
			t.Fatalf("reacquiring fixed relay port returned reply=%d and handler did not stop", rep)
		}
	}
	if relay.Port != uint16(port) {
		t.Fatalf("reacquired relay port=%d, want %d", relay.Port, port)
	}
	_ = third.Close()
	select {
	case <-thirdDone:
	case <-time.After(time.Second):
		t.Fatal("reacquired association did not stop")
	}
}

func TestUDPAssociateUsesNonContiguousRelayPortPool(t *testing.T) {
	ports := reserveNonContiguousUDPPorts(t, 2)
	srv := newUDPTestServer(config.UDP{
		Enabled: true, BindIP: "127.0.0.1", Advertise: "auto",
		RelayPorts: ports, IdleTimeout: config.Duration(5 * time.Second),
	})
	defer srv.Close()

	first, firstDone := runHandler(t, srv)
	greet(t, first)
	request(t, first, cmdUDPAssociate, net.IPv4zero, 0)
	firstRelay := readSuccessReply(t, first)
	if firstRelay.Port != ports[0] && firstRelay.Port != ports[1] {
		t.Fatalf("first relay port=%d, want one of %v", firstRelay.Port, ports)
	}

	second, secondDone := runHandler(t, srv)
	greet(t, second)
	request(t, second, cmdUDPAssociate, net.IPv4zero, 0)
	secondRelay := readSuccessReply(t, second)
	if secondRelay.Port != ports[0] && secondRelay.Port != ports[1] {
		t.Fatalf("second relay port=%d, want one of %v", secondRelay.Port, ports)
	}
	if secondRelay.Port == firstRelay.Port {
		t.Fatalf("concurrent associations both received relay port %d", firstRelay.Port)
	}

	third, thirdDone := runHandler(t, srv)
	greet(t, third)
	request(t, third, cmdUDPAssociate, net.IPv4zero, 0)
	rep, _ := readUDPTestReply(t, third)
	if rep == repSuccess {
		t.Fatal("association unexpectedly succeeded after relay port pool was exhausted")
	}
	_ = third.Close()
	select {
	case err := <-thirdDone:
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("exhausted pool handler error=%v, want EADDRINUSE", err)
		}
	case <-time.After(time.Second):
		t.Fatal("exhausted-pool association did not stop")
	}

	_ = first.Close()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first pooled association did not stop")
	}

	reacquired, reacquiredDone := runHandler(t, srv)
	greet(t, reacquired)
	request(t, reacquired, cmdUDPAssociate, net.IPv4zero, 0)
	reacquiredRelay := readSuccessReply(t, reacquired)
	if reacquiredRelay.Port != firstRelay.Port {
		t.Fatalf("reacquired relay port=%d, want released port %d", reacquiredRelay.Port, firstRelay.Port)
	}
	_ = reacquired.Close()
	select {
	case <-reacquiredDone:
	case <-time.After(time.Second):
		t.Fatal("reacquired pooled association did not stop")
	}

	_ = second.Close()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second pooled association did not stop")
	}
}

func TestUDPAssociationLimitIsReleasedWithControl(t *testing.T) {
	srv := newUDPTestServer(config.UDP{
		Enabled: true, MaxAssociations: 1, BindIP: "127.0.0.1", Advertise: "auto",
		IdleTimeout: config.Duration(5 * time.Second),
	})
	first, firstDone := runHandler(t, srv)
	greet(t, first)
	request(t, first, cmdUDPAssociate, net.IPv4zero, 0)
	_ = readSuccessReply(t, first)

	second, secondDone := runHandler(t, srv)
	greet(t, second)
	request(t, second, cmdUDPAssociate, net.IPv4zero, 0)
	rep, _ := readUDPTestReply(t, second)
	if rep != repNotAllowed {
		t.Fatalf("second association reply=%d, want not-allowed", rep)
	}
	_ = second.Close()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("limited association did not stop")
	}

	_ = first.Close()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first association did not release capacity")
	}
	third, thirdDone := runHandler(t, srv)
	greet(t, third)
	request(t, third, cmdUDPAssociate, net.IPv4zero, 0)
	_ = readSuccessReply(t, third)
	_ = third.Close()
	select {
	case <-thirdDone:
	case <-time.After(time.Second):
		t.Fatal("reacquired association did not stop")
	}
}

func TestUDPRelayRejectsWrongSourcePortAndFragments(t *testing.T) {
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	intruder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer intruder.Close()

	srv := newUDPTestServer(config.UDP{Enabled: true, StrictEndpoint: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(3 * time.Second)})
	control, done := runHandler(t, srv)
	defer control.Close()
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, client.LocalAddr().(*net.UDPAddr).Port)
	relay := readSuccessReply(t, control)
	packet := udpTestPacket(target.LocalAddr().(*net.UDPAddr), []byte("must-not-pass"))
	if _, err := intruder.WriteToUDP(packet, relay); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSMetric(t, srv, func(m MetricsSnapshot) bool { return m.UDPClientSourceDrops == 1 })

	fragment := append([]byte(nil), packet...)
	fragment[2] = 1
	if _, err := client.WriteToUDP(fragment, relay); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSMetric(t, srv, func(m MetricsSnapshot) bool { return m.UDPFragmentDrops == 1 })

	_ = target.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 128)
	if _, _, err := target.ReadFromUDP(buf); err == nil {
		t.Fatal("rejected source or fragmented datagram reached target")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatal(err)
	}

	if _, err := client.WriteToUDP(udpTestPacket(target.LocalAddr().(*net.UDPAddr), []byte("allowed")), relay); err != nil {
		t.Fatal(err)
	}
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := target.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "allowed" {
		t.Fatalf("target payload=%q", buf[:n])
	}
	_ = control.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("association did not stop")
	}
}

func TestUDPRelayAllowsReplyFromNegotiatedPortByDefault(t *testing.T) {
	initial, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	responder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()
	go func() {
		buf := make([]byte, 128)
		n, from, readErr := initial.ReadFromUDP(buf)
		if readErr == nil {
			_, _ = responder.WriteToUDP(buf[:n], from)
		}
	}()

	srv := newUDPTestServer(config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(3 * time.Second)})
	control, done := runHandler(t, srv)
	defer control.Close()
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, 0)
	relay := readSuccessReply(t, control)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.WriteToUDP(udpTestPacket(initial.LocalAddr().(*net.UDPAddr), []byte("port-switch")), relay); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 256)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	from, payload, err := parseUDPDatagram(buf[:n])
	if err != nil || from.Port != uint16(responder.LocalAddr().(*net.UDPAddr).Port) || string(payload) != "port-switch" {
		t.Fatalf("negotiated-port response from=%v payload=%q err=%v", from, payload, err)
	}
	_ = control.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("association did not stop")
	}
}

func TestUDPAdvertiseAutoPrefersConcreteBindIP(t *testing.T) {
	srv := newUDPTestServer(config.UDP{BindIP: "127.0.0.2", Advertise: "auto"})
	defer srv.Close()
	got, err := srv.udpAdvertiseIP(net.IPv4(127, 0, 0, 1))
	if err != nil || !got.Equal(net.IPv4(127, 0, 0, 2)) {
		t.Fatalf("advertise IP=%v err=%v, want concrete bind IP", got, err)
	}
}

func TestUDPAdvertiseRejectsUnusableLegacyCandidates(t *testing.T) {
	tests := []struct {
		name         string
		udp          config.UDP
		controlLocal net.IP
	}{
		{
			name:         "mapped unspecified",
			udp:          config.UDP{BindIP: "0.0.0.0", Advertise: "auto", AdvertiseMap: map[string]string{"127.0.0.1": "0.0.0.0"}},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name:         "mapped multicast",
			udp:          config.UDP{BindIP: "0.0.0.0", Advertise: "auto", AdvertiseMap: map[string]string{"127.0.0.1": "224.0.0.1"}},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name:         "explicit unspecified",
			udp:          config.UDP{BindIP: "0.0.0.0", Advertise: "0.0.0.0"},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name:         "explicit multicast",
			udp:          config.UDP{BindIP: "0.0.0.0", Advertise: "224.0.0.1"},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name:         "explicit IPv6 link-local",
			udp:          config.UDP{BindIP: "::", Advertise: "fe80::1"},
			controlLocal: net.ParseIP("::1"),
		},
		{
			name:         "concrete bind multicast",
			udp:          config.UDP{BindIP: "224.0.0.1", Advertise: "auto"},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name:         "concrete bind IPv6 link-local",
			udp:          config.UDP{BindIP: "fe80::1", Advertise: "auto"},
			controlLocal: net.ParseIP("::1"),
		},
		{
			name:         "control multicast",
			udp:          config.UDP{BindIP: "0.0.0.0", Advertise: "auto"},
			controlLocal: net.IPv4(224, 0, 0, 1),
		},
		{
			name:         "control IPv6 link-local",
			udp:          config.UDP{BindIP: "::", Advertise: "auto"},
			controlLocal: net.ParseIP("fe80::1"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newUDPTestServer(tt.udp)
			defer srv.Close()
			if got, err := srv.udpAdvertiseIP(tt.controlLocal); err == nil {
				t.Fatalf("udpAdvertiseIP()=%v, want error for legacy unusable candidate", got)
			}
		})
	}
}

func TestUDPAdvertiseValidatesCompleteLegacyConfigBeforeSelection(t *testing.T) {
	tests := []struct {
		name         string
		udp          config.UDP
		controlLocal net.IP
	}{
		{
			name: "invalid non-matching map entry",
			udp: config.UDP{
				BindIP: "0.0.0.0", Advertise: "auto",
				AdvertiseMap: map[string]string{"192.0.2.10": "0.0.0.0"},
			},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name: "bad explicit value hidden by valid matching map",
			udp: config.UDP{
				BindIP: "0.0.0.0", Advertise: "0.0.0.0",
				AdvertiseMap: map[string]string{"127.0.0.1": "127.0.0.2"},
			},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
		{
			name: "non-matching canonical duplicate",
			udp: config.UDP{
				BindIP: "::", Advertise: "auto",
				AdvertiseMap: map[string]string{
					"2001:0db8:0:0:0:0:0:1": "2001:db8::2",
					"2001:db8::1":           "2001:db8::3",
				},
			},
			controlLocal: net.ParseIP("::1"),
		},
		{
			name: "non-matching map family mismatch",
			udp: config.UDP{
				BindIP: "0.0.0.0", Advertise: "auto",
				AdvertiseMap: map[string]string{"2001:db8::1": "2001:db8::2"},
			},
			controlLocal: net.IPv4(127, 0, 0, 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newUDPTestServer(tt.udp)
			defer srv.Close()
			if got, err := srv.udpAdvertiseIP(tt.controlLocal); err == nil {
				t.Fatalf("udpAdvertiseIP()=%v, want complete legacy config validation error", got)
			}
		})
	}
}

func TestIsPublicUnicastIP(t *testing.T) {
	tests := []struct {
		ip      string
		public  bool
	}{
		{"0.0.0.0", false},
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"172.31.255.255", false},
		{"192.168.0.1", false},
		{"169.254.0.1", false},
		{"224.0.0.1", false},
		{"1.2.3.4", true},
		{"203.0.113.1", true},
		{"::1", false},
		{"fe80::1", false},
		{"fc00::1", false},
		{"ff02::1", false},
		{"2001:db8::1", true},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			got := isPublicUnicastIP(net.ParseIP(tt.ip))
			if got != tt.public {
				t.Fatalf("isPublicUnicastIP(%q)=%v, want %v", tt.ip, got, tt.public)
			}
		})
	}
}

func TestUDPAdvertiseAutoPrefersPublicInterfaceIP(t *testing.T) {
	old := publicInterfaceIPFunc
	publicInterfaceIPFunc = func(ipv4 bool) (net.IP, error) {
		return net.IPv4(203, 0, 113, 10), nil
	}
	defer func() { publicInterfaceIPFunc = old }()

	srv := newUDPTestServer(config.UDP{BindIP: "0.0.0.0", Advertise: "auto"})
	defer srv.Close()
	got, err := srv.udpAdvertiseIP(net.IPv4(10, 0, 0, 1))
	if err != nil || !got.Equal(net.IPv4(203, 0, 113, 10)) {
		t.Fatalf("advertise IP=%v err=%v, want public interface IP", got, err)
	}
}

func TestUDPAdvertiseAutoFailsWithoutPublicInterfaceIP(t *testing.T) {
	old := publicInterfaceIPFunc
	publicInterfaceIPFunc = func(ipv4 bool) (net.IP, error) {
		return nil, fmt.Errorf("no public IPv4 address found")
	}
	defer func() { publicInterfaceIPFunc = old }()

	srv := newUDPTestServer(config.UDP{BindIP: "0.0.0.0", Advertise: "auto"})
	defer srv.Close()
	if got, err := srv.udpAdvertiseIP(net.IPv4(10, 0, 0, 1)); err == nil {
		t.Fatalf("udpAdvertiseIP()=%v, want error when no public interface IP exists", got)
	}
}

func TestUDPAdvertiseAutoStillUsesPublicControlLocal(t *testing.T) {
	srv := newUDPTestServer(config.UDP{BindIP: "0.0.0.0", Advertise: "auto"})
	defer srv.Close()
	got, err := srv.udpAdvertiseIP(net.IPv4(203, 0, 113, 20))
	if err != nil || !got.Equal(net.IPv4(203, 0, 113, 20)) {
		t.Fatalf("advertise IP=%v err=%v, want control local IP", got, err)
	}
}

func TestUDPAssociateReturnsGeneralFailureForInvalidNonMatchingLegacyMap(t *testing.T) {
	srv := newUDPTestServer(config.UDP{
		Enabled: true, BindIP: "127.0.0.1", Advertise: "auto",
		AdvertiseMap: map[string]string{"127.0.0.2": "0.0.0.0"},
		IdleTimeout:  config.Duration(2 * time.Second),
	})
	defer srv.Close()
	control, done := runHandler(t, srv)
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, 0)
	rep, _ := readUDPTestReply(t, control)
	if rep != repGeneralFailure {
		t.Fatalf("reply=%d, want general-failure", rep)
	}
	_ = control.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("handler accepted invalid non-matching legacy advertise map")
		}
	case <-time.After(time.Second):
		t.Fatal("invalid legacy advertise config did not stop association")
	}
}

func TestUDPClientDomainValidationHonorsIPv4Only(t *testing.T) {
	dnsAddress, queries := startBootstrapDNS(t, [4]byte{127, 0, 0, 1})
	srv := New(config.Server{
		ConnectTimeout: config.Duration(time.Second),
		DNS:            config.DNS{IPv4Only: true, Servers: []string{dnsAddress}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	if err := srv.validateUDPClient(context.Background(), address{Host: "client.test"}, net.IPv4(127, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if queries.a.Load() == 0 || queries.aaaa.Load() != 0 {
		t.Fatalf("UDP client validation DNS queries: A=%d AAAA=%d", queries.a.Load(), queries.aaaa.Load())
	}
}

func TestUDPRelayFiltersResponsesFromTargetsClientDidNotContact(t *testing.T) {
	allowed, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Close()
	attacker, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()

	relayOutbound := make(chan *net.UDPAddr, 1)
	releaseAllowed := make(chan struct{})
	go func() {
		buf := make([]byte, 128)
		n, from, readErr := allowed.ReadFromUDP(buf)
		if readErr != nil {
			return
		}
		relayOutbound <- cloneUDPAddr(from)
		<-releaseAllowed
		_, _ = allowed.WriteToUDP(buf[:n], from)
	}()

	srv := newUDPTestServer(config.UDP{Enabled: true, StrictEndpoint: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(3 * time.Second)})
	control, done := runHandler(t, srv)
	defer control.Close()
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, 0)
	relay := readSuccessReply(t, control)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.WriteToUDP(udpTestPacket(allowed.LocalAddr().(*net.UDPAddr), []byte("good")), relay); err != nil {
		t.Fatal(err)
	}

	var outbound *net.UDPAddr
	select {
	case outbound = <-relayOutbound:
	case <-time.After(time.Second):
		t.Fatal("allowed target did not receive datagram")
	}
	if _, err := attacker.WriteToUDP([]byte("injected"), outbound); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSMetric(t, srv, func(m MetricsSnapshot) bool { return m.UDPResponseSourceDrops == 1 })
	close(releaseAllowed)

	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 128)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	from, payload, err := parseUDPDatagram(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if from.Port != uint16(allowed.LocalAddr().(*net.UDPAddr).Port) || string(payload) != "good" {
		t.Fatalf("response from=%v payload=%q", from, payload)
	}
	_ = control.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("association did not stop")
	}
}

func TestUDPRelayAppliesResolvedTargetPolicy(t *testing.T) {
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	port := target.LocalAddr().(*net.UDPAddr).Port
	cfg := config.Server{
		ConnectTimeout: config.Duration(time.Second),
		Auth:           config.Auth{Method: "none"},
		Access: config.AccessControl{
			TargetDefault: "allow",
			TargetRules:   []string{"deny 127.0.0.1:" + fmt.Sprint(port)},
		},
		UDP: config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(2 * time.Second)},
	}
	srv := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	control, done := runHandler(t, srv)
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, 0)
	relay := readSuccessReply(t, control)
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.WriteToUDP(udpTestPacket(target.LocalAddr().(*net.UDPAddr), []byte("denied")), relay); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSMetric(t, srv, func(m MetricsSnapshot) bool { return m.TargetDenied == 1 })
	_ = target.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := target.ReadFromUDP(make([]byte, 64)); err == nil {
		t.Fatal("policy-denied UDP datagram reached target")
	}
	_ = control.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("association did not stop")
	}
}

func TestUDPResolveSelectsAnAllowedAddressAfterDeniedCandidate(t *testing.T) {
	srv := New(config.Server{Access: config.AccessControl{
		TargetDefault: "allow",
		TargetRules:   []string{"deny 192.0.2.1"},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	assoc := &udpAssociation{server: srv}
	target, err := assoc.selectResolvedTarget(address{Host: "multi.example", Port: 53}, []net.IPAddr{
		{IP: net.ParseIP("192.0.2.1")},
		{IP: net.ParseIP("192.0.2.2")},
	})
	if err != nil || !target.IP.Equal(net.ParseIP("192.0.2.2")) {
		t.Fatalf("target=%v err=%v, want second allowed address", target, err)
	}
}

func TestUDPResolvedTargetsRetainAllAllowedFallbacks(t *testing.T) {
	srv := New(config.Server{Access: config.AccessControl{
		TargetDefault: "allow",
		TargetRules:   []string{"deny 192.0.2.1"},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	assoc := &udpAssociation{server: srv}
	targets, err := assoc.selectResolvedTargets(address{Host: "multi.example", Port: 53}, []net.IPAddr{
		{IP: net.ParseIP("192.0.2.1")},
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("192.0.2.2")},
	})
	if err != nil || len(targets) != 2 || targets[0].IP.To4() != nil || !targets[1].IP.Equal(net.ParseIP("192.0.2.2")) {
		t.Fatalf("targets=%v err=%v, want both allowed candidates in resolver order", targets, err)
	}
}

func TestUDPSendFallsBackAfterUnreachableAddressFamily(t *testing.T) {
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	srv := newUDPTestServer(config.UDP{StrictEndpoint: true})
	defer srv.Close()
	assoc := &udpAssociation{
		server: srv, targetTTL: time.Minute,
		allowedTargets: make(map[netip.AddrPort]*udpTargetGrant),
		writeTarget: func(conn *net.UDPConn, payload []byte, target *net.UDPAddr) (int, error) {
			if target.IP.To4() == nil {
				return 0, syscall.ENETUNREACH
			}
			return conn.WriteToUDP(payload, target)
		},
	}
	defer assoc.close()
	targets := []*net.UDPAddr{
		{IP: net.ParseIP("fe80::1"), Zone: "wwan-proxy-missing-zone", Port: 9},
		target.LocalAddr().(*net.UDPAddr),
	}
	if !assoc.sendToTargets(context.Background(), []byte("fallback"), targets) {
		t.Fatal("no UDP target candidate succeeded")
	}
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := target.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "fallback" {
		t.Fatalf("fallback target payload=%q err=%v", buf[:n], err)
	}
}

func TestUDPFailedConcurrentGrantDoesNotRevokeCommittedTarget(t *testing.T) {
	srv := newUDPTestServer(config.UDP{})
	defer srv.Close()
	assoc := &udpAssociation{
		server: srv, targetTTL: time.Minute,
		allowedTargets: make(map[netip.AddrPort]*udpTargetGrant),
	}
	target := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 69}
	committed := assoc.beginTargetGrant(target)
	assoc.finishTargetGrant(committed, true)
	failed := assoc.beginTargetGrant(&net.UDPAddr{IP: target.IP, Port: 40000})
	assoc.finishTargetGrant(failed, false)
	if !assoc.targetAllowed(&net.UDPAddr{IP: target.IP, Port: 50000}, time.Now()) {
		t.Fatal("failed concurrent send revoked an earlier committed IP grant")
	}

	pendingA := assoc.beginTargetGrant(target)
	pendingB := assoc.beginTargetGrant(target)
	assoc.finishTargetGrant(pendingA, false)
	if !assoc.targetAllowed(target, time.Now()) {
		t.Fatal("one failed send revoked another pending grant")
	}
	assoc.finishTargetGrant(pendingB, false)
	if !assoc.targetAllowed(target, time.Now()) {
		t.Fatal("pending failures revoked the earlier committed grant")
	}
}

func TestUDPAssociationCancellationClosesSocketsBeforeWaitingForWrites(t *testing.T) {
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := cloneUDPAddr(relay.LocalAddr().(*net.UDPAddr))
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = relay.Close()
		t.Fatal(err)
	}
	defer client.Close()

	srv := newUDPTestServer(config.UDP{StrictEndpoint: true})
	defer srv.Close()
	writeStarted := make(chan struct{})
	assoc := &udpAssociation{
		server: srv, client: relay, peerIP: cloneUDPAddr(client.LocalAddr().(*net.UDPAddr)).IP,
		targetTTL: time.Minute, allowedTargets: make(map[netip.AddrPort]*udpTargetGrant),
		writeTarget: func(conn *net.UDPConn, _ []byte, _ *net.UDPAddr) (int, error) {
			close(writeStarted)
			var one [1]byte
			_, _, readErr := conn.ReadFromUDP(one[:])
			return 0, readErr
		},
	}
	assoc.lastUnix.Store(time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- assoc.loop(ctx, 10*time.Second) }()
	if _, err := client.WriteToUDP(udpTestPacket(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, []byte("blocked")), relayAddr); err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("outbound write did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("association cancellation returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("association cancellation waited on a blocked UDP operation")
	}
	rebound, err := net.ListenUDP("udp4", relayAddr)
	if err != nil {
		t.Fatalf("client relay port was not released after cancellation: %v", err)
	}
	_ = rebound.Close()
}

func TestUDPGrantSteadyStateOnlyPrunesCurrentKey(t *testing.T) {
	srv := newUDPTestServer(config.UDP{StrictEndpoint: true})
	defer srv.Close()
	now := time.Now()
	current := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 53}
	stale := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 53}
	assoc := &udpAssociation{
		server: srv, targetTTL: time.Minute,
		allowedTargets: map[netip.AddrPort]*udpTargetGrant{
			udpAddrPort(current): {committedUntil: now.Add(time.Minute), pending: make(map[uint64]time.Time)},
			udpAddrPort(stale):   {committedUntil: now.Add(-time.Minute), pending: make(map[uint64]time.Time)},
		},
	}
	token := assoc.beginTargetGrant(current)
	assoc.finishTargetGrant(token, true)
	if _, exists := assoc.allowedTargets[udpAddrPort(stale)]; !exists {
		t.Fatal("steady-state grant update scanned and pruned an unrelated key")
	}
}

func TestUDPGrantCapacityPrunesExpiredEntriesBeforeEviction(t *testing.T) {
	srv := newUDPTestServer(config.UDP{StrictEndpoint: true})
	defer srv.Close()
	assoc := &udpAssociation{
		server: srv, targetTTL: time.Minute,
		allowedTargets: make(map[netip.AddrPort]*udpTargetGrant, udpMaxTargets),
	}
	for i := 0; i < udpMaxTargets; i++ {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		assoc.allowedTargets[netip.AddrPortFrom(ip, 53)] = &udpTargetGrant{
			committedUntil: time.Now().Add(-time.Minute), pending: make(map[uint64]time.Time),
		}
	}
	target := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 53}
	token := assoc.beginTargetGrant(target)
	assoc.finishTargetGrant(token, true)
	if len(assoc.allowedTargets) != 1 || !assoc.targetAllowed(target, time.Now()) {
		t.Fatalf("capacity cleanup retained %d grants, want only the new committed target", len(assoc.allowedTargets))
	}
}

func TestUDPAssociateIPv6(t *testing.T) {
	echo, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 128)
		for {
			n, from, readErr := echo.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], from)
		}
	}()

	srv := newUDPTestServer(config.UDP{Enabled: true, BindIP: "::1", Advertise: "auto", IdleTimeout: config.Duration(2 * time.Second)})
	control, done := runUDPHandlerNetwork(t, srv, "tcp6", "[::1]:0")
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv6zero, 0)
	relay := readSuccessReply(t, control)
	if relay.IP.To4() != nil || !relay.IP.Equal(net.IPv6loopback) {
		t.Fatalf("IPv6 relay address=%v", relay)
	}
	client, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.WriteToUDP(udpTestPacket(echo.LocalAddr().(*net.UDPAddr), []byte("v6")), relay); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 128)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := parseUDPDatagram(buf[:n])
	if err != nil || string(payload) != "v6" {
		t.Fatalf("IPv6 payload=%q error=%v", payload, err)
	}
	_ = control.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("IPv6 association did not stop")
	}
}

func TestUDPAssociateRejectsRelayAddressFamilyMismatch(t *testing.T) {
	srv := newUDPTestServer(config.UDP{Enabled: true, BindIP: "::1", Advertise: "auto"})
	control, done := runHandler(t, srv)
	greet(t, control)
	request(t, control, cmdUDPAssociate, net.IPv4zero, 0)
	rep, _ := readUDPTestReply(t, control)
	if rep != repAddressNotSupported {
		t.Fatalf("reply=%d, want address-not-supported", rep)
	}
	_ = control.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mismatched association did not stop")
	}
}

func TestUDPAddrPortNormalizesIPv4MappedAddress(t *testing.T) {
	v4 := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
	mapped := &net.UDPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 53}
	if udpAddrPort(v4) != udpAddrPort(mapped) {
		t.Fatalf("IPv4 address keys differ: %v != %v", udpAddrPort(v4), udpAddrPort(mapped))
	}
}

func TestUDPAssociationPinsClientIPAndPort(t *testing.T) {
	assoc := &udpAssociation{peerIP: net.IPv4(127, 0, 0, 1)}
	if assoc.acceptClient(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 41000}) {
		t.Fatal("association accepted a UDP datagram from a different client IP")
	}
	if !assoc.acceptClient(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000}) {
		t.Fatal("association rejected its first matching client endpoint")
	}
	if assoc.acceptClient(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41001}) {
		t.Fatal("association accepted a different client source port after pinning")
	}
}

func newUDPTestServer(udp config.UDP) *Server {
	return New(config.Server{
		ConnectTimeout: config.Duration(time.Second),
		Auth:           config.Auth{Method: "none"},
		UDP:            udp,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func udpTestPacket(target *net.UDPAddr, payload []byte) []byte {
	return append(appendAddress([]byte{0, 0, 0}, target.IP, target.Port), payload...)
}

func readUDPTestReply(t *testing.T, conn net.Conn) (byte, address) {
	t.Helper()
	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		t.Fatal(err)
	}
	if head[0] != version5 || head[2] != 0 {
		t.Fatalf("reply header=%v", head)
	}
	addr, err := readAddress(conn)
	if err != nil {
		t.Fatal(err)
	}
	return head[1], addr
}

func runUDPHandlerNetwork(t *testing.T, srv *Server, network, address string) (net.Conn, <-chan error) {
	t.Helper()
	ln, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("%s unavailable: %v", network, err)
	}
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		_ = ln.Close()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		done <- srv.handle(conn)
	}()
	conn, err := net.Dial(network, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return conn, done
}

func TestParseUDPDatagramRejectsFragments(t *testing.T) {
	_, _, err := parseUDPDatagram([]byte{0, 0, 1, atypIPv4, 127, 0, 0, 1, 0, 53})
	if !errors.Is(err, errFragmentedUDP) {
		t.Fatalf("error=%v, want fragmented UDP", err)
	}
}
