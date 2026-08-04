package httpproxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wwan-proxy/internal/config"
)

type DialContext func(context.Context, string, string) (net.Conn, error)

type MetricsSnapshot struct {
	ActiveRequests int64  `json:"active_requests"`
	TotalRequests  uint64 `json:"total_requests"`
	RequestErrors  uint64 `json:"request_errors"`
	HTTPRequests   uint64 `json:"http_requests"`
	ConnectTunnels uint64 `json:"connect_tunnels"`
	UploadBytes    uint64 `json:"upload_bytes"`
	DownloadBytes  uint64 `json:"download_bytes"`
}

type metricCounters struct {
	activeRequests atomic.Int64
	totalRequests  atomic.Uint64
	requestErrors  atomic.Uint64
	httpRequests   atomic.Uint64
	connectTunnels atomic.Uint64
	uploadBytes    atomic.Uint64
	downloadBytes  atomic.Uint64
}

type Server struct {
	cfg       config.Server
	log       *slog.Logger
	dial      DialContext
	transport *http.Transport
	http      *http.Server
	sem       chan struct{}

	mu       sync.Mutex
	listener net.Listener
	active   map[net.Conn]struct{}
	closing  bool
	metrics  metricCounters
}

func New(cfg config.Server, logger *slog.Logger, dial DialContext) *Server {
	s := &Server{cfg: cfg, log: logger.With("server", cfg.Name, "interface", cfg.Interface), dial: dial, active: make(map[net.Conn]struct{})}
	if cfg.MaxConnections > 0 {
		s.sem = make(chan struct{}, cfg.MaxConnections)
	}
	timeout := cfg.ConnectTimeout.Value(10 * time.Second)
	s.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       cfg.IdleTimeout.Value(5 * time.Minute),
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: timeout,
		IdleTimeout:       cfg.IdleTimeout.Value(5 * time.Minute),
		MaxHeaderBytes:    1 << 20,
	}
	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	ln, err := lc.Listen(ctx, "tcp", s.cfg.HTTPProxy.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.HTTPProxy.Listen, err)
	}
	return s.Serve(ctx, ln)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	s.log.Info("HTTP/HTTPS proxy listening", "address", ln.Addr())
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.http.Close()
		case <-done:
		}
	}()
	err := s.http.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	s.transport.CloseIdleConnections()
	s.mu.Lock()
	s.closing = true
	for conn := range s.active {
		_ = conn.Close()
	}
	s.mu.Unlock()
	return s.http.Close()
}

func (s *Server) Metrics() MetricsSnapshot {
	m := &s.metrics
	return MetricsSnapshot{
		ActiveRequests: m.activeRequests.Load(), TotalRequests: m.totalRequests.Load(), RequestErrors: m.requestErrors.Load(),
		HTTPRequests: m.httpRequests.Load(), ConnectTunnels: m.connectTunnels.Load(),
		UploadBytes: m.uploadBytes.Load(), DownloadBytes: m.downloadBytes.Load(),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.metrics.totalRequests.Add(1)
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			s.metrics.requestErrors.Add(1)
			w.Header().Set("Connection", "close")
			http.Error(w, "proxy connection limit reached", http.StatusServiceUnavailable)
			return
		}
	}
	s.metrics.activeRequests.Add(1)
	defer s.metrics.activeRequests.Add(-1)
	if !s.authorize(w, r) {
		s.metrics.requestErrors.Add(1)
		return
	}
	r.Header.Del("Proxy-Authorization")
	var err error
	if r.Method == http.MethodConnect {
		s.metrics.connectTunnels.Add(1)
		err = s.handleConnect(w, r)
	} else {
		s.metrics.httpRequests.Add(1)
		err = s.handleHTTP(w, r)
	}
	if err != nil && !normalClose(err) {
		s.metrics.requestErrors.Add(1)
		s.log.Warn("HTTP proxy request ended with error", "remote", r.RemoteAddr, "method", r.Method, "target", r.Host, "error", err)
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Auth.Method != "username_password" {
		return true
	}
	scheme, encoded, ok := strings.Cut(r.Header.Get("Proxy-Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		proxyAuthRequired(w)
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		proxyAuthRequired(w)
		return false
	}
	username, password, ok := strings.Cut(string(raw), ":")
	expected, exists := s.cfg.Auth.Users[username]
	if !ok || !exists || subtle.ConstantTimeCompare([]byte(expected), []byte(password)) != 1 {
		proxyAuthRequired(w)
		return false
	}
	return true
}

func proxyAuthRequired(w http.ResponseWriter) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="wwan-proxy", charset="UTF-8"`)
	http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) error {
	if !r.URL.IsAbs() || !strings.EqualFold(r.URL.Scheme, "http") || r.URL.Host == "" || r.URL.User != nil {
		http.Error(w, "HTTP proxy requests must use an absolute http:// URI; use CONNECT for HTTPS", http.StatusBadRequest)
		return fmt.Errorf("invalid absolute request target %q", r.RequestURI)
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = cloneURL(r.URL)
	// RFC 9112 requires a proxy to derive Host from an absolute-form target
	// instead of trusting a conflicting Host header.
	out.Host = out.URL.Host
	out.Header = r.Header.Clone()
	removeHopByHop(out.Header)
	out.Header.Del("Proxy-Authorization")
	appendVia(out.Header)
	if r.Body != nil {
		out.Body = &countingReadCloser{ReadCloser: r.Body, counter: &s.metrics.uploadBytes}
	}
	resp, err := s.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return fmt.Errorf("round trip %s: %w", out.URL.Redacted(), err)
	}
	defer resp.Body.Close()
	removeHopByHop(resp.Header)
	appendVia(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	n, copyErr := io.Copy(w, resp.Body)
	s.metrics.downloadBytes.Add(uint64(n))
	return copyErr
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) error {
	target, err := normalizeAuthority(r.RequestURI, "443")
	if err != nil {
		http.Error(w, "invalid CONNECT authority", http.StatusBadRequest)
		return err
	}
	if r.Host != "" {
		hostTarget, hostErr := normalizeAuthority(r.Host, "443")
		if hostErr != nil || hostTarget != target {
			http.Error(w, "CONNECT Host does not match request authority", http.StatusBadRequest)
			return fmt.Errorf("CONNECT authority %q does not match Host %q", r.RequestURI, r.Host)
		}
	}
	upstream, err := s.dial(r.Context(), "tcp", target)
	if err != nil {
		http.Error(w, "upstream connection failed", http.StatusBadGateway)
		return fmt.Errorf("connect %s: %w", target, err)
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "CONNECT tunneling is unavailable", http.StatusInternalServerError)
		return errors.New("response writer does not support hijacking")
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return err
	}
	defer client.Close()
	defer upstream.Close()
	if !s.track(client, upstream) {
		return net.ErrClosed
	}
	defer s.untrack(client, upstream)
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\nProxy-Agent: wwan-proxy\r\n\r\n"); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	bufferedClient := &bufferedConn{Conn: client, reader: rw.Reader}
	s.log.Debug("HTTP CONNECT", "client", r.RemoteAddr, "destination", target)
	return relayTCP(bufferedClient, upstream, s.cfg.IdleTimeout.Value(5*time.Minute), &s.metrics.uploadBytes, &s.metrics.downloadBytes)
}

func (s *Server) track(conns ...net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		for _, conn := range conns {
			_ = conn.Close()
		}
		return false
	}
	for _, conn := range conns {
		s.active[conn] = struct{}{}
	}
	return true
}

func (s *Server) untrack(conns ...net.Conn) {
	s.mu.Lock()
	for _, conn := range conns {
		delete(s.active, conn)
	}
	s.mu.Unlock()
}

func normalizeAuthority(authority, defaultPort string) (string, error) {
	if authority == "" || strings.ContainsAny(authority, "/?#") {
		return "", fmt.Errorf("invalid authority %q", authority)
	}
	u, err := url.Parse("//" + authority)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("invalid authority %q", authority)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return "", fmt.Errorf("invalid authority port %q", port)
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func removeHopByHop(header http.Header) {
	for _, connection := range header.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

func appendVia(header http.Header) {
	const value = "1.1 wwan-proxy"
	if previous := header.Get("Via"); previous != "" {
		header.Set("Via", previous+", "+value)
	} else {
		header.Set("Via", value)
	}
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func cloneURL(src *url.URL) *url.URL {
	clone := *src
	return &clone
}

type countingReadCloser struct {
	io.ReadCloser
	counter *atomic.Uint64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.counter.Add(uint64(n))
	return n, err
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

type deadlineConn struct {
	net.Conn
	idle time.Duration
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

func (c *deadlineConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func relayTCP(client, upstream net.Conn, idle time.Duration, upload, download *atomic.Uint64) error {
	client = &deadlineConn{Conn: client, idle: idle}
	upstream = &deadlineConn{Conn: upstream, idle: idle}
	errCh := make(chan error, 2)
	copyOne := func(dst, src net.Conn, counter *atomic.Uint64) {
		n, err := io.Copy(dst, src)
		counter.Add(uint64(n))
		if closer, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		errCh <- err
	}
	go copyOne(upstream, client, upload)
	go copyOne(client, upstream, download)
	first := <-errCh
	if first != nil {
		_ = client.Close()
		_ = upstream.Close()
	}
	second := <-errCh
	if first != nil {
		return first
	}
	return second
}

func normalClose(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}
