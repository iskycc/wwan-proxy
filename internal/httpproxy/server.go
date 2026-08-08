package httpproxy

import (
	"bufio"
	"context"
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
	"wwan-proxy/internal/netrelay"
	"wwan-proxy/internal/policy"
	"wwan-proxy/internal/proxyauth"
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
	AdmissionDrops uint64 `json:"admission_drops"`
	LimitDrops     uint64 `json:"connection_limit_drops"`
	TargetDenied   uint64 `json:"target_denied"`
}

type metricCounters struct {
	activeRequests atomic.Int64
	totalRequests  atomic.Uint64
	requestErrors  atomic.Uint64
	httpRequests   atomic.Uint64
	connectTunnels atomic.Uint64
	uploadBytes    atomic.Uint64
	downloadBytes  atomic.Uint64
	admissionDrops atomic.Uint64
	limitDrops     atomic.Uint64
	targetDenied   atomic.Uint64
}

type Server struct {
	cfg       config.Server
	log       *slog.Logger
	dial      DialContext
	transport *http.Transport
	http      *http.Server
	limiter   *policy.Limiter
	access    *policy.Access
	clients   *policy.IPLimiter

	mu           sync.Mutex
	listener     net.Listener
	active       map[net.Conn]struct{}
	closing      bool
	ready        chan struct{}
	readyOnce    sync.Once
	handlerCount int
	handlerDone  chan struct{}
	finishOnce   sync.Once
	metrics      metricCounters
}

func New(cfg config.Server, logger *slog.Logger, dial DialContext) *Server {
	return NewWithLimiter(cfg, logger, dial, policy.NewLimiter(cfg.MaxConnections))
}

func NewWithLimiter(cfg config.Server, logger *slog.Logger, dial DialContext, limiter *policy.Limiter) *Server {
	return NewWithLimiters(cfg, logger, dial, limiter, policy.NewIPLimiter(cfg.Access.MaxConnectionsPerIP))
}

// NewWithLimiters accepts shared instance-wide capacity controls.
func NewWithLimiters(cfg config.Server, logger *slog.Logger, dial DialContext, limiter *policy.Limiter, clients *policy.IPLimiter) *Server {
	handlerDone := make(chan struct{})
	close(handlerDone)
	s := &Server{
		cfg: cfg, log: logger.With("server", cfg.Name, "interface", cfg.Interface), dial: dial,
		active: make(map[net.Conn]struct{}), ready: make(chan struct{}), handlerDone: handlerDone,
	}
	s.access, _ = policy.NewAccess(cfg.Access)
	s.clients = clients
	s.limiter = limiter
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
	defer ln.Close()
	return s.Serve(ctx, ln)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
	s.log.Info("HTTP/HTTPS proxy listening", "address", ln.Addr())
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-done:
		}
	}()
	err := s.http.Serve(ln)
	close(done)
	s.mu.Lock()
	if s.listener == ln {
		s.listener = nil
	}
	s.mu.Unlock()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closing = true
	ln := s.listener
	for conn := range s.active {
		_ = conn.Close()
	}
	handlersDone := s.handlerDone
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	err := s.http.Close()
	<-handlersDone
	s.finish()
	return err
}

// Ready is closed after the listening socket has been installed successfully.
// It allows the manager to distinguish a usable replacement from an async bind
// failure during a configuration handoff.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// StopAccepting releases only the listening socket. Established HTTP requests
// and CONNECT tunnels remain usable while a replacement binds the same port.
func (s *Server) StopAccepting() {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// GracefulClose drains ordinary HTTP requests via http.Server.Shutdown and
// separately waits for hijacked CONNECT handlers, which net/http no longer owns.
func (s *Server) GracefulClose(ctx context.Context) error {
	s.StopAccepting()
	shutdownErr := s.http.Shutdown(ctx)
	s.mu.Lock()
	s.closing = true
	done := s.handlerDone
	s.mu.Unlock()
	select {
	case <-done:
		s.finish()
		return shutdownErr
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

func (s *Server) finish() {
	s.finishOnce.Do(func() { s.transport.CloseIdleConnections() })
}

func (s *Server) Metrics() MetricsSnapshot {
	m := &s.metrics
	return MetricsSnapshot{
		ActiveRequests: m.activeRequests.Load(), TotalRequests: m.totalRequests.Load(), RequestErrors: m.requestErrors.Load(),
		HTTPRequests: m.httpRequests.Load(), ConnectTunnels: m.connectTunnels.Load(),
		UploadBytes: m.uploadBytes.Load(), DownloadBytes: m.downloadBytes.Load(),
		AdmissionDrops: m.admissionDrops.Load(), LimitDrops: m.limitDrops.Load(), TargetDenied: m.targetDenied.Load(),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.beginHandler() {
		w.Header().Set("Connection", "close")
		http.Error(w, "proxy is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.endHandler()
	s.metrics.totalRequests.Add(1)
	remote := tcpAddress(r.RemoteAddr)
	if s.access != nil && !s.access.AllowClient(remote) {
		s.metrics.admissionDrops.Add(1)
		w.Header().Set("Connection", "close")
		http.Error(w, "proxy client is not allowed", http.StatusForbidden)
		return
	}
	releaseClient, allowed := s.clients.Acquire(remote)
	if !allowed {
		s.metrics.limitDrops.Add(1)
		w.Header().Set("Connection", "close")
		http.Error(w, "proxy per-IP connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer releaseClient()
	releaseCapacity, allowed := s.limiter.Acquire()
	if !allowed {
		s.metrics.requestErrors.Add(1)
		s.metrics.limitDrops.Add(1)
		w.Header().Set("Connection", "close")
		http.Error(w, "proxy connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer releaseCapacity()
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

func (s *Server) beginHandler() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	if s.handlerCount == 0 {
		s.handlerDone = make(chan struct{})
	}
	s.handlerCount++
	return true
}

func (s *Server) endHandler() {
	s.mu.Lock()
	s.handlerCount--
	if s.handlerCount == 0 {
		close(s.handlerDone)
	}
	s.mu.Unlock()
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
	if !ok || !proxyauth.VerifyUser(s.cfg.Auth.Users, username, password) {
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
		if errors.Is(err, policy.ErrTargetDenied) {
			s.metrics.targetDenied.Add(1)
			http.Error(w, "proxy target is not allowed", http.StatusForbidden)
			return err
		}
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return fmt.Errorf("round trip %s: %w", out.URL.Redacted(), err)
	}
	defer resp.Body.Close()
	removeHopByHop(resp.Header)
	appendVia(resp.Header)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(&atomicCountingWriter{Writer: w, counter: &s.metrics.downloadBytes}, resp.Body)
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
		if errors.Is(err, policy.ErrTargetDenied) {
			s.metrics.targetDenied.Add(1)
			http.Error(w, "proxy target is not allowed", http.StatusForbidden)
			return fmt.Errorf("connect %s: %w", target, err)
		}
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

func tcpAddress(value string) net.Addr {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return stringAddress(value)
	}
	n, _ := strconv.Atoi(port)
	return &net.TCPAddr{IP: net.ParseIP(host), Port: n}
}

type stringAddress string

func (a stringAddress) Network() string { return "tcp" }
func (a stringAddress) String() string  { return string(a) }

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

// atomicCountingWriter updates metrics for every successful write. This keeps
// both forwarded responses and tunnels observable before the stream closes.
type atomicCountingWriter struct {
	io.Writer
	counter *atomic.Uint64
}

func (w *atomicCountingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.counter.Add(uint64(n))
	}
	return n, err
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
func (c *bufferedConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func relayTCP(client, upstream net.Conn, idle time.Duration, upload, download *atomic.Uint64) error {
	return netrelay.Bidirectional(client, upstream, idle, upload, download)
}

func normalClose(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}
