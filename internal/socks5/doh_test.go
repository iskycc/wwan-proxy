package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestDoHResolvesEndpointThroughBootstrapDNS(t *testing.T) {
	var requests atomic.Int32
	bootstrapAddress, bootstrapQueries := startBootstrapDNS(t, [4]byte{127, 0, 0, 1})
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
		URL: u.String(), BootstrapIPs: []string{bootstrapAddress},
		Timeout: config.Duration(2 * time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

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
	if bootstrapQueries.total.Load() == 0 {
		t.Fatal("DoH endpoint was not resolved through the bootstrap DNS server")
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
	badBootstrap, _ := startBootstrapDNS(t, [4]byte{127, 0, 0, 2})
	goodBootstrap, _ := startBootstrapDNS(t, [4]byte{127, 0, 0, 1})
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URL: u.String(), BootstrapIPs: []string{badBootstrap, goodBootstrap},
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
	bootstrapAddress, _ := startBootstrapDNS(t, [4]byte{127, 0, 0, 1})
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URL: u.String(), BootstrapIPs: []string{bootstrapAddress},
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

func TestIPv4OnlyDialDoesNotSendAAAAQuery(t *testing.T) {
	var aRequests atomic.Int32
	var aaaaRequests atomic.Int32
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
		switch queryType {
		case 1:
			aRequests.Add(1)
		case 28:
			aaaaRequests.Add(1)
		}
		answer, err := dnsTestResponseIPv4(query, [4]byte{127, 0, 0, 1})
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
	bootstrapAddress, bootstrapQueries := startBootstrapDNS(t, [4]byte{127, 0, 0, 1})
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	srv := New(config.Server{DNS: config.DNS{IPv4Only: true, DoH: &config.DoH{
		URL: u.String(), BootstrapIPs: []string{bootstrapAddress},
		Timeout: config.Duration(time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, targetPort, err := net.SplitHostPort(target.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := srv.DialContext(ctx, "tcp", net.JoinHostPort("example.test", targetPort))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case acceptedConn := <-accepted:
		_ = acceptedConn.Close()
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if aRequests.Load() == 0 {
		t.Fatal("IPv4-only dial did not issue an A query")
	}
	if got := aaaaRequests.Load(); got != 0 {
		t.Fatalf("IPv4-only dial issued %d AAAA queries", got)
	}
	if bootstrapQueries.a.Load() == 0 {
		t.Fatal("IPv4-only DoH endpoint did not use bootstrap DNS")
	}
	if got := bootstrapQueries.aaaa.Load(); got != 0 {
		t.Fatalf("IPv4-only DoH endpoint issued %d bootstrap AAAA queries", got)
	}
}

func TestIPv4OnlyDoHPreservesEndpointAndBootstrapDNSError(t *testing.T) {
	closedListener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bootstrapAddress := closedListener.LocalAddr().String()
	_ = closedListener.Close()
	endpoint := "https://resolver.invalid/dns-query"
	srv := New(config.Server{DNS: config.DNS{IPv4Only: true, DoH: &config.DoH{
		URL: endpoint, BootstrapIPs: []string{bootstrapAddress}, Timeout: config.Duration(time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = srv.DialContext(ctx, "tcp", "example.test:443")
	if err == nil {
		t.Fatal("expected DoH failure")
	}
	message := err.Error()
	for _, detail := range []string{"DoH " + endpoint, "bootstrap DNS " + bootstrapAddress, "resolve resolver.invalid", "resolve IPv4 example.test"} {
		if !strings.Contains(message, detail) {
			t.Fatalf("detailed DoH error is missing %q: %v", detail, err)
		}
	}
}

func TestDoHProvidersRaceAndFastestValidResponseWins(t *testing.T) {
	var slowRequests atomic.Int32
	var fastRequests atomic.Int32
	var invalidRequests atomic.Int32
	newProvider := func(delay time.Duration, ip [4]byte, requests *atomic.Int32) *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			query, _ := io.ReadAll(r.Body)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			answer, err := dnsTestResponseIPv4(query, ip)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(answer)
		}))
	}
	slow := newProvider(300*time.Millisecond, [4]byte{192, 0, 2, 10}, &slowRequests)
	defer slow.Close()
	fast := newProvider(15*time.Millisecond, [4]byte{192, 0, 2, 20}, &fastRequests)
	defer fast.Close()
	invalid := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invalidRequests.Add(1)
		query, _ := io.ReadAll(r.Body)
		answer, _ := dnsTestResponseIPv4(query, [4]byte{192, 0, 2, 99})
		binary.BigEndian.PutUint16(answer[2:4], 0x8182) // SERVFAIL must not win the race.
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	}))
	defer invalid.Close()
	srv := New(config.Server{DNS: config.DNS{IPv4Only: true, DoH: &config.DoH{
		URLs: []string{slow.URL, invalid.URL, fast.URL}, Timeout: config.Duration(time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	started := time.Now()
	ips, err := srv.doh.lookupIPv4(context.Background(), "race.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.IPv4(192, 0, 2, 20)) {
		t.Fatalf("fast provider did not win: %v", ips)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("provider requests were not raced: %v", elapsed)
	}
	// The losing slow request may be canceled during TLS before its HTTP
	// handler runs. The immediate invalid provider and valid provider must both
	// be observed, proving an invalid fast response does not terminate the race.
	if invalidRequests.Load() != 1 || fastRequests.Load() != 1 || slowRequests.Load() > 1 {
		t.Fatalf("unexpected provider requests: slow=%d invalid=%d fast=%d", slowRequests.Load(), invalidRequests.Load(), fastRequests.Load())
	}
}

func TestDoHNegativeCacheUsesSOAMinimumTTL(t *testing.T) {
	query, err := buildDNSAQuery("missing.test")
	if err != nil {
		t.Fatal(err)
	}
	response, err := dnsTestNXDomainResponse(query, 120, 30)
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := dnsResponseCacheTTL(response)
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 30 {
		t.Fatalf("negative cache TTL=%d, want min(SOA TTL 120, SOA.MINIMUM 30)", ttl)
	}
}

func TestDoHNegativeResponseIsCached(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query, _ := io.ReadAll(r.Body)
		response, _ := dnsTestNXDomainResponse(query, 120, 30)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(response)
	}))
	defer provider.Close()
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URLs: []string{provider.URL}, Timeout: config.Duration(time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	query, _ := buildDNSAQuery("negative-cache.test")
	for range 2 {
		response, err := srv.doh.query(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(response[2:4])&0x000f != 3 {
			t.Fatal("cached response lost NXDOMAIN RCODE")
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("negative response made %d requests, want 1", requests.Load())
	}
}

func TestDoHCacheUsesTTLAndRewritesQueryID(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query, _ := io.ReadAll(r.Body)
		answer, err := dnsTestResponseIPv4TTL(query, [4]byte{192, 0, 2, 30}, 60)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	}))
	defer provider.Close()
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URLs: []string{provider.URL}, Timeout: config.Duration(time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	query, err := buildDNSAQuery("cache.test")
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(query[:2], 100)
	first, err := srv.doh.query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	key, _, _ := dnsCacheKey(query)
	srv.doh.cacheMu.Lock()
	entry := srv.doh.cache[key]
	entry.storedAt = entry.storedAt.Add(-2 * time.Second)
	srv.doh.cache[key] = entry
	srv.doh.cacheMu.Unlock()
	secondQuery, _ := buildDNSAQuery("CaChE.TeSt")
	binary.BigEndian.PutUint16(secondQuery[:2], 200)
	second, err := srv.doh.query(context.Background(), secondQuery)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("unexpired result was queried again: requests=%d", requests.Load())
	}
	if binary.BigEndian.Uint16(second[:2]) != 200 {
		t.Fatalf("cached response ID=%d, want 200", binary.BigEndian.Uint16(second[:2]))
	}
	firstLayout, _ := parseDNSLayout(first)
	secondLayout, _ := parseDNSLayout(second)
	if secondLayout.questions[0].name != "CaChE.TeSt" {
		t.Fatalf("cached response question=%q, want current query capitalization", secondLayout.questions[0].name)
	}
	if firstLayout.answers[0].ttl != 60 || secondLayout.answers[0].ttl != 58 {
		t.Fatalf("TTL was not aged: first=%d second=%d", firstLayout.answers[0].ttl, secondLayout.answers[0].ttl)
	}
	srv.doh.cacheMu.Lock()
	entry = srv.doh.cache[key]
	entry.expires = time.Now().Add(-time.Second)
	srv.doh.cache[key] = entry
	srv.doh.cacheMu.Unlock()
	if _, err = srv.doh.query(context.Background(), secondQuery); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("expired result was not refreshed: requests=%d", requests.Load())
	}
}

func TestDoHCacheCoalescesConcurrentMisses(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query, _ := io.ReadAll(r.Body)
		time.Sleep(40 * time.Millisecond)
		answer, _ := dnsTestResponseIPv4(query, [4]byte{192, 0, 2, 40})
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(answer)
	}))
	defer provider.Close()
	srv := New(config.Server{DNS: config.DNS{DoH: &config.DoH{
		URLs: []string{provider.URL}, Timeout: config.Duration(time.Second), InsecureSkipVerify: true,
	}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer srv.Close()
	query, _ := buildDNSAQuery("coalesce.test")
	start := make(chan struct{})
	errorsSeen := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			localQuery := append([]byte(nil), query...)
			binary.BigEndian.PutUint16(localQuery[:2], uint16(id+1))
			response, err := srv.doh.query(context.Background(), localQuery)
			if err == nil && binary.BigEndian.Uint16(response[:2]) != uint16(id+1) {
				err = errors.New("coalesced response used another query ID")
			}
			errorsSeen <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("concurrent cache misses made %d upstream requests, want 1", requests.Load())
	}
}

type bootstrapDNSCounts struct {
	total atomic.Int32
	a     atomic.Int32
	aaaa  atomic.Int32
}

func startBootstrapDNS(t *testing.T, ip [4]byte) (string, *bootstrapDNSCounts) {
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
			queryType, queryErr := dnsTestQueryType(packet[:n])
			if queryErr != nil {
				continue
			}
			response, responseErr := dnsTestResponseIPv4(packet[:n], ip)
			if responseErr != nil {
				continue
			}
			queries.total.Add(1)
			switch queryType {
			case 1:
				queries.a.Add(1)
			case 28:
				queries.aaaa.Add(1)
			}
			_, _ = conn.WriteTo(response, peer)
		}
	}()
	return conn.LocalAddr().String(), &queries
}

func dnsTestResponse(query []byte) ([]byte, error) {
	return dnsTestResponseIPv4(query, [4]byte{192, 0, 2, 123})
}

func dnsTestResponseIPv4(query []byte, ip [4]byte) ([]byte, error) {
	return dnsTestResponseIPv4TTL(query, ip, 60)
}

func dnsTestResponseIPv4TTL(query []byte, ip [4]byte, ttl uint32) ([]byte, error) {
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
	resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1)
	resp = binary.BigEndian.AppendUint32(resp, ttl)
	resp = append(resp, 0, 4, ip[0], ip[1], ip[2], ip[3])
	return resp, nil
}

func dnsTestNXDomainResponse(query []byte, soaTTL, minimum uint32) ([]byte, error) {
	_, questionEnd, err := dnsTestQuestion(query)
	if err != nil {
		return nil, err
	}
	response := append([]byte(nil), query[:questionEnd]...)
	binary.BigEndian.PutUint16(response[2:4], 0x8183)
	binary.BigEndian.PutUint16(response[6:8], 0)
	binary.BigEndian.PutUint16(response[8:10], 1)
	binary.BigEndian.PutUint16(response[10:12], 0)
	response = append(response, 0xc0, 0x0c, 0, 6, 0, 1)
	response = binary.BigEndian.AppendUint32(response, soaTTL)
	response = append(response, 0, 22, 0, 0) // RDLENGTH, root MNAME, root RNAME
	for _, value := range []uint32{1, 2, 3, 4, minimum} {
		response = binary.BigEndian.AppendUint32(response, value)
	}
	return response, nil
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
