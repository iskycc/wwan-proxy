package httpproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
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

func TestHTTPForwardingHeadersAndMetrics(t *testing.T) {
	var gotVia, gotHop, gotProxyAuth, gotBody, gotHost string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVia = r.Header.Get("Via")
		gotHop = r.Header.Get("X-Remove-Me")
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotHost = r.Host
		w.Header().Set("X-Origin", "yes")
		_, _ = w.Write([]byte("forwarded:" + r.URL.Path))
	}))
	defer origin.Close()

	proxy, proxyURL := startTestProxy(t, config.Server{Auth: config.Auth{Method: "none"}})
	defer proxy.Close()
	client := proxyClient(proxyURL, nil)
	req, err := http.NewRequest(http.MethodPost, origin.URL+"/resource", strings.NewReader("request-body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "X-Remove-Me")
	req.Header.Set("X-Remove-Me", "secret")
	req.Header.Set("Proxy-Authorization", "should-not-leak")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "forwarded:/resource" || resp.Header.Get("X-Origin") != "yes" {
		t.Fatalf("status=%d body=%q headers=%v", resp.StatusCode, body, resp.Header)
	}
	if gotVia != "1.1 wwan-proxy" || gotHop != "" || gotProxyAuth != "" || gotBody != "request-body" || gotHost != strings.TrimPrefix(origin.URL, "http://") {
		t.Fatalf("via=%q hop=%q proxy_auth=%q body=%q host=%q", gotVia, gotHop, gotProxyAuth, gotBody, gotHost)
	}
	if resp.Header.Get("Via") != "1.1 wwan-proxy" {
		t.Fatalf("response Via=%q", resp.Header.Get("Via"))
	}
	metrics := proxy.Metrics()
	if metrics.HTTPRequests != 1 || metrics.ConnectTunnels != 0 || metrics.UploadBytes != uint64(len("request-body")) || metrics.DownloadBytes == 0 || metrics.ActiveRequests != 0 {
		t.Fatalf("unexpected metrics %+v", metrics)
	}
}

func TestHTTPDownloadMetricsUpdateBeforeResponseEnds(t *testing.T) {
	firstChunk := []byte("streaming-response-chunk")
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	firstSent := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(firstChunk)
		w.(http.Flusher).Flush()
		close(firstSent)
		<-release
		_, _ = w.Write([]byte("tail"))
	}))
	defer origin.Close()

	proxy, proxyURL := startTestProxy(t, config.Server{Auth: config.Auth{Method: "none"}})
	client := proxyClient(proxyURL, nil)
	type requestResult struct {
		response *http.Response
		err      error
	}
	resultCh := make(chan requestResult, 1)
	go func() {
		response, err := client.Get(origin.URL)
		resultCh <- requestResult{response: response, err: err}
	}()

	select {
	case <-firstSent:
	case <-time.After(time.Second):
		t.Fatal("origin did not start streaming")
	}
	waitForHTTPMetric(t, proxy, func(metrics MetricsSnapshot) bool {
		return metrics.DownloadBytes >= uint64(len(firstChunk)) && metrics.ActiveRequests == 1
	})

	close(release)
	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	_, _ = io.Copy(io.Discard, got.response.Body)
	_ = got.response.Body.Close()
}

func TestAbsoluteTargetOverridesConflictingHost(t *testing.T) {
	gotHost := make(chan string, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost <- r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	proxy := New(config.Server{Auth: config.Auth{Method: "none"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), (&net.Dialer{}).DialContext)
	req := httptest.NewRequest(http.MethodGet, origin.URL+"/host-check", nil)
	req.RequestURI = origin.URL + "/host-check"
	req.Host = "attacker.invalid"
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if host := <-gotHost; host != strings.TrimPrefix(origin.URL, "http://") {
		t.Fatalf("origin received Host %q", host)
	}
}

func TestHTTPSConnectTunnel(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure-through-connect"))
	}))
	defer origin.Close()
	proxy, proxyURL := startTestProxy(t, config.Server{Auth: config.Auth{Method: "none"}})
	defer proxy.Close()
	client := proxyClient(proxyURL, &tls.Config{InsecureSkipVerify: true}) // test server certificate
	client.Transport.(*http.Transport).DisableKeepAlives = true
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "secure-through-connect" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	var metrics MetricsSnapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		metrics = proxy.Metrics()
		if metrics.ActiveRequests == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if metrics.ConnectTunnels != 1 || metrics.UploadBytes == 0 || metrics.DownloadBytes == 0 || metrics.ActiveRequests != 0 {
		t.Fatalf("unexpected CONNECT metrics %+v", metrics)
	}
}

func TestConnectRelayMetricsUpdateBeforeTunnelEnds(t *testing.T) {
	var upload, download atomic.Uint64
	client, relayClient := net.Pipe()
	relayUpstream, upstream := net.Pipe()
	defer client.Close()
	defer upstream.Close()

	done := make(chan error, 1)
	go func() { done <- relayTCP(relayClient, relayUpstream, time.Minute, &upload, &download) }()

	uploadPayload := []byte("connect-upload")
	go func() { _, _ = client.Write(uploadPayload) }()
	if _, err := io.ReadFull(upstream, make([]byte, len(uploadPayload))); err != nil {
		t.Fatal(err)
	}
	waitForAtomicCounter(t, &upload, uint64(len(uploadPayload)))

	downloadPayload := []byte("connect-download")
	go func() { _, _ = upstream.Write(downloadPayload) }()
	if _, err := io.ReadFull(client, make([]byte, len(downloadPayload))); err != nil {
		t.Fatal(err)
	}
	waitForAtomicCounter(t, &download, uint64(len(downloadPayload)))

	_ = client.Close()
	_ = upstream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CONNECT relay did not stop")
	}
}

func TestProxyBasicAuthenticationForHTTPAndHTTPS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("Proxy-Authorization leaked to origin")
		}
		_, _ = w.Write([]byte("authenticated"))
	}))
	defer origin.Close()
	cfg := config.Server{Auth: config.Auth{Method: "username_password", Users: map[string]string{"alice": "correct horse"}}}
	proxy, proxyURL := startTestProxy(t, cfg)
	defer proxy.Close()

	unauthenticated := proxyClient(proxyURL, nil)
	resp, err := unauthenticated.Get("http://example.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired || !strings.HasPrefix(resp.Header.Get("Proxy-Authenticate"), "Basic ") {
		t.Fatalf("status=%d challenge=%q", resp.StatusCode, resp.Header.Get("Proxy-Authenticate"))
	}

	authURL := *proxyURL
	authURL.User = url.UserPassword("alice", "correct horse")
	authenticated := proxyClient(&authURL, &tls.Config{InsecureSkipVerify: true}) // test server certificate
	resp, err = authenticated.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "authenticated" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if proxy.Metrics().RequestErrors != 1 {
		t.Fatalf("unexpected authentication metrics %+v", proxy.Metrics())
	}
}

func TestConcurrentHTTPForwarding(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		_, _ = fmt.Fprintf(w, "ok:%s", r.URL.Query().Get("id"))
	}))
	defer origin.Close()
	proxy, proxyURL := startTestProxy(t, config.Server{Auth: config.Auth{Method: "none"}, MaxConnections: 256})
	defer proxy.Close()
	client := proxyClient(proxyURL, nil)
	const requests = 100
	var wg sync.WaitGroup
	errors := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("%s/?id=%d", origin.URL, id))
			if err != nil {
				errors <- err
				return
			}
			_, readErr := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK {
				errors <- fmt.Errorf("status=%d read=%v", resp.StatusCode, readErr)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	metrics := proxy.Metrics()
	if metrics.TotalRequests != requests || metrics.HTTPRequests != requests || metrics.RequestErrors != 0 || metrics.ActiveRequests != 0 {
		t.Fatalf("unexpected concurrent metrics %+v", metrics)
	}
}

func TestInvalidTargetsAndAuthority(t *testing.T) {
	proxy := New(config.Server{Auth: config.Auth{Method: "none"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), (&net.Dialer{}).DialContext)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/origin-form", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("origin-form status=%d", recorder.Code)
	}
	for _, authority := range []string{"", "user@example.com:443", "example.com:70000", "example.com/path"} {
		if _, err := normalizeAuthority(authority, "443"); err == nil {
			t.Fatalf("accepted authority %q", authority)
		}
	}
	if got, err := normalizeAuthority("[::1]", "443"); err != nil || got != "[::1]:443" {
		t.Fatalf("IPv6 authority got=%q err=%v", got, err)
	}
	mismatch := httptest.NewRecorder()
	proxy.ServeHTTP(mismatch, &http.Request{Method: http.MethodConnect, RequestURI: "example.com:443", Host: "other.example:443", Header: make(http.Header)})
	if mismatch.Code != http.StatusBadRequest {
		t.Fatalf("mismatched CONNECT Host status=%d", mismatch.Code)
	}
}

func startTestProxy(t *testing.T, cfg config.Server) (*Server, *url.URL) {
	t.Helper()
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = config.Duration(2 * time.Second)
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = config.Duration(2 * time.Second)
	}
	proxy := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), (&net.Dialer{Timeout: 2 * time.Second}).DialContext)
	ts := httptest.NewServer(proxy)
	t.Cleanup(func() {
		ts.Close()
		_ = proxy.Close()
	})
	proxyURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return proxy, proxyURL
}

func proxyClient(proxyURL *url.URL, tlsConfig *tls.Config) *http.Client {
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: tlsConfig,
		DialContext:     (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func waitForHTTPMetric(t *testing.T, proxy *Server, condition func(MetricsSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition(proxy.Metrics()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("live HTTP metrics did not update, latest=%+v", proxy.Metrics())
}

func waitForAtomicCounter(t *testing.T, counter *atomic.Uint64, expected uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if counter.Load() == expected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("live tunnel metric=%d, want %d", counter.Load(), expected)
}

func TestDialContextIsUsed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer origin.Close()
	called := make(chan string, 1)
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	proxy := New(config.Server{Auth: config.Auth{Method: "none"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ctx context.Context, network, address string) (net.Conn, error) {
		called <- address
		return dialer.DialContext(ctx, network, address)
	})
	ts := httptest.NewServer(proxy)
	defer ts.Close()
	proxyURL, _ := url.Parse(ts.URL)
	resp, err := proxyClient(proxyURL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case address := <-called:
		if address != strings.TrimPrefix(origin.URL, "http://") {
			t.Fatalf("dialed %q", address)
		}
	case <-time.After(time.Second):
		t.Fatal("custom outbound dialer was not used")
	}
}

func TestCloseTerminatesHijackedConnectTunnel(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	go func() {
		conn, acceptErr := origin.Accept()
		if acceptErr == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}
	}()

	proxy, proxyURL := startTestProxy(t, config.Server{Auth: config.Auth{Method: "none"}})
	conn, err := net.DialTimeout("tcp", proxyURL.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", origin.Addr(), origin.Addr())
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		t.Fatalf("CONNECT status=%q err=%v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("CONNECT client remained open after proxy close")
	}
	deadline := time.Now().Add(time.Second)
	for proxy.Metrics().ActiveRequests != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if proxy.Metrics().ActiveRequests != 0 {
		t.Fatalf("CONNECT handler remained active: %+v", proxy.Metrics())
	}
}

func TestGracefulCloseDrainsHijackedConnectTunnel(t *testing.T) {
	origin := listenEcho(t)
	defer origin.Close()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := New(config.Server{
		Auth: config.Auth{Method: "none"}, IdleTimeout: config.Duration(time.Minute),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), (&net.Dialer{Timeout: time.Second}).DialContext)
	serveDone := make(chan error, 1)
	go func() { serveDone <- proxy.Serve(context.Background(), ln) }()
	<-proxy.Ready()

	client, err := net.DialTimeout("tcp4", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", origin.Addr(), origin.Addr())
	reader := bufio.NewReader(client)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}

	proxy.StopAccepting()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP listener did not stop accepting")
	}
	payload := []byte("tunnel-survives-listener-handoff")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("tunnel returned %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	gracefulDone := make(chan error, 1)
	go func() { gracefulDone <- proxy.GracefulClose(ctx) }()
	select {
	case err := <-gracefulDone:
		t.Fatalf("graceful close returned before tunnel drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = client.Close()
	select {
	case err := <-gracefulDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("graceful close did not finish after tunnel closed")
	}
}

func listenEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	return ln
}
