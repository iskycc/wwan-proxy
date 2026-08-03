package socks5

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
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

func dnsTestResponse(query []byte) ([]byte, error) {
	if len(query) < 17 {
		return nil, io.ErrUnexpectedEOF
	}
	off := 12
	for {
		if off >= len(query) {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(query[off])
		off++
		if n == 0 {
			break
		}
		if n&0xc0 != 0 || off+n > len(query) {
			return nil, io.ErrUnexpectedEOF
		}
		off += n
	}
	if off+4 > len(query) {
		return nil, io.ErrUnexpectedEOF
	}
	qtype := binary.BigEndian.Uint16(query[off : off+2])
	questionEnd := off + 4
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
