package socks5

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestConnect(t *testing.T) {
	echo := listenTCPEcho(t)
	defer echo.Close()
	srv := New(config.Server{
		ConnectTimeout: config.Duration(2 * time.Second),
		IdleTimeout:    config.Duration(2 * time.Second),
		Auth:           config.Auth{Method: "none"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, done := runHandler(t, srv)
	defer client.Close()

	greet(t, client)
	dst := echo.Addr().(*net.TCPAddr)
	request(t, client, cmdConnect, dst.IP, dst.Port)
	readSuccessReply(t, client)
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(client, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop")
	}
}

func TestRelayMetricsUpdateBeforeStreamEnds(t *testing.T) {
	srv := New(config.Server{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, relayClient := net.Pipe()
	relayUpstream, upstream := net.Pipe()
	defer client.Close()
	defer upstream.Close()

	done := make(chan error, 1)
	go func() { done <- srv.relayTCP(relayClient, relayUpstream, time.Minute) }()

	upload := []byte("upload-while-open")
	go func() { _, _ = client.Write(upload) }()
	gotUpload := make([]byte, len(upload))
	if _, err := io.ReadFull(upstream, gotUpload); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSMetric(t, srv, func(metrics MetricsSnapshot) bool {
		return metrics.TCPUploadBytes == uint64(len(upload))
	})

	download := []byte("download-while-open")
	go func() { _, _ = upstream.Write(download) }()
	gotDownload := make([]byte, len(download))
	if _, err := io.ReadFull(client, gotDownload); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSMetric(t, srv, func(metrics MetricsSnapshot) bool {
		return metrics.TCPDownloadBytes == uint64(len(download))
	})

	// The streams are deliberately still open when both assertions run.
	_ = client.Close()
	_ = upstream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop")
	}
}

func waitForSOCKSMetric(t *testing.T, srv *Server, condition func(MetricsSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition(srv.Metrics()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("live SOCKS metrics were not updated before stream close")
}

func TestUDPAssociate(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(b)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(b[:n], from)
		}
	}()
	srv := New(config.Server{
		ConnectTimeout: config.Duration(2 * time.Second),
		Auth:           config.Auth{Method: "none"},
		UDP:            config.UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto", IdleTimeout: config.Duration(2 * time.Second)},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, done := runHandler(t, srv)
	defer client.Close()
	greet(t, client)
	request(t, client, cmdUDPAssociate, net.IPv4zero, 0)
	relay := readSuccessReply(t, client)
	if relay.Port < udpRelayPortMin || relay.Port > udpRelayPortMax {
		t.Fatalf("relay port %d is outside %d-%d", relay.Port, udpRelayPortMin, udpRelayPortMax)
	}

	udpClient, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	target := echo.LocalAddr().(*net.UDPAddr)
	packet := appendAddress([]byte{0, 0, 0}, target.IP, target.Port)
	packet = append(packet, []byte("dns-like-payload")...)
	if _, err := udpClient.WriteToUDP(packet, relay); err != nil {
		t.Fatal(err)
	}
	_ = udpClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 2048)
	n, _, err := udpClient.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	from, payload, err := parseUDPDatagram(b[:n])
	if err != nil {
		t.Fatal(err)
	}
	if from.Port != uint16(target.Port) || string(payload) != "dns-like-payload" {
		t.Fatalf("unexpected response from=%v payload=%q", from, payload)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop")
	}
}

func TestPasswordAuthentication(t *testing.T) {
	srv := New(config.Server{Auth: config.Auth{Method: "username_password", Users: map[string]string{"alice": "secret"}}}, slog.Default())
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.negotiate(server) }()
	_, _ = client.Write([]byte{5, 1, 2})
	b := make([]byte, 2)
	_, _ = io.ReadFull(client, b)
	if b[1] != 2 {
		t.Fatalf("method=%d", b[1])
	}
	_, _ = client.Write(append([]byte{1, 5}, append([]byte("alice"), append([]byte{6}, []byte("secret")...)...)...))
	_, _ = io.ReadFull(client, b)
	if b[1] != 0 {
		t.Fatalf("status=%d", b[1])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func listenTCPEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln
}

func runHandler(t *testing.T, srv *Server) (net.Conn, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		_ = ln.Close()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		done <- srv.handle(c)
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c, done
}

func greet(t *testing.T, c net.Conn) {
	t.Helper()
	_, _ = c.Write([]byte{5, 1, 0})
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	if b[0] != 5 || b[1] != 0 {
		t.Fatalf("greeting response %v", b)
	}
}

func request(t *testing.T, c net.Conn, command byte, ip net.IP, port int) {
	t.Helper()
	b := appendAddress([]byte{5, command, 0}, ip, port)
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func readSuccessReply(t *testing.T, c net.Conn) *net.UDPAddr {
	t.Helper()
	var h [3]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		t.Fatal(err)
	}
	if h[0] != 5 || h[1] != 0 || h[2] != 0 {
		t.Fatalf("reply header %v", h)
	}
	a, err := readAddress(c)
	if err != nil {
		t.Fatal(err)
	}
	return &net.UDPAddr{IP: net.ParseIP(a.Host), Port: int(a.Port)}
}
