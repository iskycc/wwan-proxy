package socks5

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestDoHUsesBootstrapAddress(t *testing.T) {
	var requests atomic.Int32
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "bad DoH request", http.StatusBadRequest)
			return
		}
		query, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		answer, err := dnsTestResponse(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	}))
	defer dohServer.Close()

	u, err := url.Parse(dohServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "resolver.invalid:" + u.Port()
	u.Path = "/dns-query"
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URL: u.String(), BootstrapIPs: []string{"127.0.0.1"},
		Timeout: config.Duration(2 * time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := srv.resolver.LookupHost(ctx, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || ips[0] != "192.0.2.123" {
		t.Fatalf("unexpected result %v", ips)
	}
	if requests.Load() == 0 {
		t.Fatal("DoH server was not used")
	}
}

func TestDoHFailsOverAfterTLSFailure(t *testing.T) {
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		answer, err := dnsTestResponse(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	}))
	defer dohServer.Close()
	u, err := url.Parse(dohServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	badTLS, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", u.Port()))
	if err != nil {
		t.Skipf("cannot create alternate loopback listener: %v", err)
	}
	defer badTLS.Close()
	go func() {
		for {
			conn, acceptErr := badTLS.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	u.Host = "resolver.invalid:" + u.Port()
	u.Path = "/dns-query"
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URL: u.String(), BootstrapIPs: []string{"127.0.0.2", "127.0.0.1"},
		Timeout: config.Duration(2 * time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := srv.resolver.LookupIP(ctx, "ip4", "example.test")
	if err != nil {
		t.Fatalf("DoH did not fail over after TLS failure: %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.IPv4(192, 0, 2, 123)) {
		t.Fatalf("unexpected result %v", ips)
	}
}

func TestDoHReturnsIPv4WhenParallelIPv6QueryTimesOut(t *testing.T) {
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		queryType, err := dnsTestQueryType(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if queryType == 28 {
			<-r.Context().Done()
			return
		}
		answer, err := dnsTestResponse(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	}))
	defer dohServer.Close()
	u, err := url.Parse(dohServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = "resolver.invalid:" + u.Port()
	u.Path = "/dns-query"
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URL: u.String(), BootstrapIPs: []string{"127.0.0.1"},
		Timeout: config.Duration(100 * time.Millisecond), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	ips, err := srv.resolver.LookupHost(ctx, "example.test")
	if err != nil {
		t.Fatalf("usable IPv4 answer was discarded after IPv6 timeout: %v", err)
	}
	if len(ips) != 1 || ips[0] != "192.0.2.123" {
		t.Fatalf("unexpected result %v", ips)
	}
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("configured DoH timeout was not honored: %v", elapsed)
	}
}

func TestDoHAndConnectTimeoutsHaveIndependentBudgets(t *testing.T) {
	srv := New(config.Server{
		ConnectTimeout: config.Duration(3 * time.Second),
		DNS: config.DNS{DoH: &config.DoH{
			URL: "https://resolver.invalid/dns-query", BootstrapIPs: []string{"192.0.2.53"},
			Timeout: config.Duration(7 * time.Second),
		}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	if got := srv.dialer().Timeout; got != 10*time.Second {
		t.Fatalf("dial timeout=%v, want independent DNS+connect budget of 10s", got)
	}
	if got := srv.resolutionTimeout(); got != 7*time.Second {
		t.Fatalf("resolution timeout=%v, want 7s", got)
	}
}

func dnsTestResponse(query []byte) ([]byte, error) {
	qtype, questionEnd, err := dnsTestQuestion(query)
	if err != nil {
		return nil, err
	}
	resp := append([]byte(nil), query[:questionEnd]...)
	binary.BigEndian.PutUint16(resp[2:4], 0x8180)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	if qtype != 1 {
		binary.BigEndian.PutUint16(resp[6:8], 0)
		return resp, nil
	}
	binary.BigEndian.PutUint16(resp[6:8], 1)
	resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 123)
	return resp, nil
}

func dnsTestQueryType(query []byte) (uint16, error) {
	qtype, _, err := dnsTestQuestion(query)
	return qtype, err
}

func dnsTestQuestion(query []byte) (uint16, int, error) {
	if len(query) < 17 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	off := 12
	for {
		if off >= len(query) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		n := int(query[off])
		off++
		if n == 0 {
			break
		}
		if n&0xc0 != 0 || off+n > len(query) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		off += n
	}
	if off+4 > len(query) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	qtype := binary.BigEndian.Uint16(query[off : off+2])
	return qtype, off + 4, nil
}
