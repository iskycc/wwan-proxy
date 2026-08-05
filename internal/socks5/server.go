package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/policy"
	"wwan-proxy/internal/proxyauth"
)

var (
	errAddressType   = errors.New("address type not supported")
	errFragmentedUDP = errors.New("fragmented UDP is not supported")
)

type Server struct {
	cfg      config.Server
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	resolver *net.Resolver
	doh      *dohResolver
	// resolverClose releases persistent DoH connections during hot reload and shutdown.
	resolverClose func()

	mu               sync.Mutex
	finishOnce       sync.Once
	readyOnce        sync.Once
	ready            chan struct{}
	active           map[net.Conn]struct{}
	acceptingStopped bool
	closing          bool
	wg               sync.WaitGroup
	limiter          *policy.Limiter
	access           *policy.Access
	clients          *policy.IPLimiter
	udpClients       *policy.IPLimiter
	udpLimiter       *policy.Limiter
	metrics          metricCounters
}

func New(cfg config.Server, logger *slog.Logger) *Server {
	return NewWithLimiter(cfg, logger, policy.NewLimiter(cfg.MaxConnections))
}

// NewWithLimiter allows the SOCKS5 and HTTP listeners of one configured
// instance to share the same total connection budget.
func NewWithLimiter(cfg config.Server, logger *slog.Logger, limiter *policy.Limiter) *Server {
	return NewWithLimiters(cfg, logger, limiter, policy.NewIPLimiter(cfg.Access.MaxConnectionsPerIP), policy.NewIPLimiter(cfg.Access.MaxUDPAssociationsPerIP))
}

// NewWithLimiters allows all protocol listeners belonging to one configured
// instance to share both total and per-source-IP budgets.
func NewWithLimiters(cfg config.Server, logger *slog.Logger, limiter *policy.Limiter, clients, udpClients *policy.IPLimiter) *Server {
	udpLimit := cfg.UDP.MaxAssociations
	if udpLimit == 0 {
		udpLimit = 64
	}
	return NewWithAllLimiters(cfg, logger, limiter, clients, udpClients, policy.NewLimiter(udpLimit))
}

// NewWithAllLimiters additionally accepts the instance-wide UDP association
// budget. The manager reuses it across listener generations during hot reload.
func NewWithAllLimiters(cfg config.Server, logger *slog.Logger, limiter *policy.Limiter, clients, udpClients *policy.IPLimiter, udpLimiter *policy.Limiter) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, log: logger.With("server", cfg.Name, "interface", cfg.Interface),
		ctx: ctx, cancel: cancel, active: make(map[net.Conn]struct{}), ready: make(chan struct{}),
	}
	s.access, _ = policy.NewAccess(cfg.Access)
	s.clients = clients
	s.udpClients = udpClients
	s.udpLimiter = udpLimiter
	s.limiter = limiter
	s.resolver = s.newResolver()
	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	if s.closing || s.acceptingStopped {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
	defer func() {
		_ = ln.Close()
		s.mu.Lock()
		if s.listener == ln {
			s.listener = nil
		}
		s.mu.Unlock()
	}()
	s.log.Info("SOCKS5 listening", "address", ln.Addr())
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				s.log.Warn("temporary accept error", "error", err)
				continue
			}
			return err
		}
		if s.access != nil && !s.access.AllowClient(conn.RemoteAddr()) {
			s.metrics.admissionDrops.Add(1)
			s.log.Warn("SOCKS5 client rejected by admission policy", "remote", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		releaseClient, allowed := s.clients.Acquire(conn.RemoteAddr())
		if !allowed {
			s.metrics.connectionLimitDrops.Add(1)
			s.log.Warn("per-IP connection limit reached", "remote", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		releaseCapacity, allowed := s.limiter.Acquire()
		if !allowed {
			s.metrics.connectionLimitDrops.Add(1)
			s.log.Warn("connection limit reached", "remote", conn.RemoteAddr())
			_ = conn.Close()
			releaseClient()
			continue
		}
		s.mu.Lock()
		if s.closing || s.acceptingStopped {
			s.mu.Unlock()
			_ = conn.Close()
			releaseClient()
			releaseCapacity()
			continue
		}
		s.active[conn] = struct{}{}
		s.wg.Add(1)
		s.metrics.totalConnections.Add(1)
		s.metrics.activeConnections.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer releaseClient()
			defer releaseCapacity()
			defer s.metrics.activeConnections.Add(-1)
			defer conn.Close()
			defer s.track(conn, false)
			if err := s.handle(conn); err != nil && !isNormalClose(err) {
				s.metrics.connectionErrors.Add(1)
				s.log.Warn("SOCKS5 connection ended with error", "remote", conn.RemoteAddr(), "error", err)
			}
		}()
	}
}

func (s *Server) Close() error {
	s.cancel()
	s.mu.Lock()
	s.acceptingStopped = true
	s.closing = true
	ln := s.listener
	for c := range s.active {
		_ = c.Close()
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	s.finish()
	return nil
}

// Ready is closed after the listening socket has been installed successfully.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// StopAccepting releases the listener without disrupting established
// sessions. It is used for configuration handoff before a replacement
// instance binds the same address.
func (s *Server) StopAccepting() {
	s.mu.Lock()
	s.acceptingStopped = true
	ln := s.listener
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

func (s *Server) GracefulClose(ctx context.Context) error {
	s.StopAccepting()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.cancel()
		s.finish()
		return nil
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

func (s *Server) finish() {
	s.finishOnce.Do(func() {
		if s.resolverClose != nil {
			s.resolverClose()
		}
	})
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.active[c] = struct{}{}
	} else {
		delete(s.active, c)
	}
}

func (s *Server) trackOutbound(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		_ = c.Close()
		return false
	}
	s.active[c] = struct{}{}
	return true
}

func (s *Server) handle(c net.Conn) error {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := s.negotiate(c); err != nil {
		return err
	}
	var head [3]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[0] != version5 || head[2] != 0 {
		return fmt.Errorf("invalid request header")
	}
	dst, err := readAddress(c)
	if err != nil {
		if errors.Is(err, errAddressType) {
			_ = writeReply(c, repAddressNotSupported, nil)
		}
		return err
	}
	_ = c.SetDeadline(time.Time{})
	switch head[1] {
	case cmdConnect:
		s.metrics.connectCommands.Add(1)
		return s.handleConnect(c, dst)
	case cmdBind:
		if !s.cfg.Bind.Enabled {
			_ = writeReply(c, repCommandNotSupported, nil)
			return fmt.Errorf("BIND disabled")
		}
		if s.cfg.Upstream.Enabled {
			_ = writeReply(c, repCommandNotSupported, nil)
			return fmt.Errorf("BIND not supported with upstream proxy")
		}
		s.metrics.bindCommands.Add(1)
		return s.handleBindContext(s.ctx, c, dst)
	case cmdUDPAssociate:
		if !s.cfg.UDP.Enabled {
			_ = writeReply(c, repCommandNotSupported, nil)
			return fmt.Errorf("UDP ASSOCIATE disabled")
		}
		if s.cfg.Upstream.Enabled {
			_ = writeReply(c, repCommandNotSupported, nil)
			return fmt.Errorf("UDP ASSOCIATE not supported with upstream proxy")
		}
		s.metrics.udpAssociations.Add(1)
		return s.handleUDPContext(s.ctx, c, dst)
	default:
		_ = writeReply(c, repCommandNotSupported, nil)
		return fmt.Errorf("unsupported command %d", head[1])
	}
}

func (s *Server) negotiate(c net.Conn) error {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return err
	}
	if h[0] != version5 || h[1] == 0 {
		return fmt.Errorf("invalid greeting")
	}
	methods := make([]byte, int(h[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	want := byte(methodNone)
	if s.cfg.Auth.Method == "username_password" {
		want = methodPassword
	}
	found := false
	for _, m := range methods {
		if m == want {
			found = true
			break
		}
	}
	if !found {
		_, _ = c.Write([]byte{version5, methodReject})
		return fmt.Errorf("no acceptable authentication method")
	}
	if _, err := c.Write([]byte{version5, want}); err != nil {
		return err
	}
	if want == methodPassword {
		return s.passwordAuth(c)
	}
	return nil
}

func (s *Server) passwordAuth(c net.Conn) error {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return err
	}
	if h[0] != 1 || h[1] == 0 {
		return fmt.Errorf("invalid password authentication request")
	}
	u := make([]byte, int(h[1]))
	if _, err := io.ReadFull(c, u); err != nil {
		return err
	}
	var plen [1]byte
	if _, err := io.ReadFull(c, plen[:]); err != nil {
		return err
	}
	p := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(c, p); err != nil {
		return err
	}
	valid := proxyauth.VerifyUser(s.cfg.Auth.Users, string(u), string(p))
	status := byte(0)
	if !valid {
		status = 1
	}
	_, _ = c.Write([]byte{1, status})
	if !valid {
		return fmt.Errorf("authentication failed")
	}
	return nil
}

func (s *Server) dialer() *net.Dialer {
	timeout := s.cfg.ConnectTimeout.Value(10 * time.Second)
	// net.Dialer.Timeout includes DNS resolution. Give DoH its own configured
	// budget so it cannot consume the entire TCP connection budget and collapse
	// the useful upstream error into a generic "lookup: i/o timeout".
	if s.cfg.DNS.DoH != nil {
		timeout += s.cfg.DNS.DoH.Timeout.Value(10 * time.Second)
	}
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Resolver:  s.resolver,
		Control:   bindToDevice(s.cfg.Interface),
	}
}

// DialContext resolves and dials an outbound target through the configured
// interface. Resolution is performed explicitly so destination ACLs are
// evaluated against both the requested hostname and every resolved IP; this
// prevents a hostname from bypassing a CIDR deny through DNS rebinding.
func (s *Server) DialContext(parent context.Context, network, address string) (net.Conn, error) {
	return s.dialContext(parent, network, address, true)
}

// ProbeDialContext uses the same route-bound resolver and sockets as proxy
// traffic, but intentionally bypasses client target ACLs. Manager health probes
// must test the configured egress itself even when clients use default-deny ACLs.
func (s *Server) ProbeDialContext(parent context.Context, network, address string) (net.Conn, error) {
	return s.dialContext(parent, network, address, false)
}

func (s *Server) dialContext(parent context.Context, network, address string, enforceAccess bool) (net.Conn, error) {
	ctx, cancel := s.operationContext(parent)
	defer cancel()

	if s.cfg.Upstream.Enabled {
		// Access control is still evaluated against the resolved/requested target
		// before we hand the address to the upstream SOCKS5 proxy.
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("invalid target port %q", port)
		}
		if enforceAccess && s.access != nil {
			literalHost, _ := splitIPZone(host)
			if ip := net.ParseIP(literalHost); ip != nil {
				if !s.access.AllowTarget(host, ip, portNumber) {
					return nil, fmt.Errorf("%w: %s", policy.ErrTargetDenied, address)
				}
			}
		}
		return DialViaUpstream(ctx, s.cfg.Upstream, s.dialer(), network, address)
	}

	dialer := s.dialer()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("invalid target port %q", port)
	}
	literalHost, zone := splitIPZone(host)
	if ip := net.ParseIP(literalHost); ip != nil {
		if enforceAccess && s.access != nil && !s.access.AllowTarget(host, ip, portNumber) {
			return nil, fmt.Errorf("%w: %s", policy.ErrTargetDenied, address)
		}
		if zone != "" {
			literalHost += "%" + zone
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(literalHost, port))
	}

	lookupCtx, lookupCancel := context.WithTimeout(ctx, dialer.Timeout)
	defer lookupCancel()
	ips, err := s.lookupTargetIPs(lookupCtx, network, host)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("lookup %s: no address", host)
	}
	allowed := ips[:0]
	for _, ip := range ips {
		if !enforceAccess || s.access == nil || s.access.AllowTarget(host, ip.IP, portNumber) {
			allowed = append(allowed, ip)
		}
	}
	if enforceAccess && len(allowed) == 0 {
		return nil, fmt.Errorf("%w: %s resolved only to denied addresses", policy.ErrTargetDenied, address)
	}

	connectDialer := s.dialerWithoutResolver()
	var failures []error
	for i, ip := range allowed {
		attemptContext, attemptCancel := dividedAttemptContext(lookupCtx, len(allowed)-i)
		connectNetwork := networkForIP(network, ip.IP)
		ipHost := ip.IP.String()
		if ip.Zone != "" {
			ipHost += "%" + ip.Zone
		}
		conn, dialErr := connectDialer.DialContext(attemptContext, connectNetwork, net.JoinHostPort(ipHost, port))
		attemptCancel()
		if dialErr == nil {
			return conn, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", ip.String(), dialErr))
	}
	return nil, fmt.Errorf("connect %s: %w", address, errors.Join(failures...))
}

func (s *Server) lookupTargetIPs(ctx context.Context, network, host string) ([]net.IPAddr, error) {
	if s.cfg.DNS.IPv4Only || strings.HasSuffix(network, "4") {
		resolved, err := s.lookupIPv4(ctx, host)
		ips := make([]net.IPAddr, 0, len(resolved))
		for _, ip := range resolved {
			ips = append(ips, net.IPAddr{IP: ip})
		}
		return ips, err
	}
	ips, err := s.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(network, "6") {
		filtered := ips[:0]
		for _, ip := range ips {
			if ip.IP.To4() == nil {
				filtered = append(filtered, ip)
			}
		}
		ips = filtered
	}
	return ips, nil
}

func splitIPZone(host string) (string, string) {
	if i := strings.LastIndexByte(host, '%'); i > 0 {
		return host[:i], host[i+1:]
	}
	return host, ""
}

func networkForIP(network string, ip net.IP) string {
	if network != "tcp" && network != "udp" {
		return network
	}
	if ip.To4() != nil {
		return network + "4"
	}
	return network + "6"
}

// operationContext makes every outbound operation obey both its caller's
// deadline and the server instance lifetime. This is important during hot
// reload: closing the instance must interrupt DNS and connect attempts rather
// than waiting for their independent timeouts.
func (s *Server) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (s *Server) lookupIPv4(ctx context.Context, host string) ([]net.IP, error) {
	if s.doh != nil {
		return s.doh.lookupIPv4(ctx, host)
	}
	return s.resolver.LookupIP(ctx, "ip4", host)
}

func (s *Server) resolutionTimeout() time.Duration {
	if s.cfg.DNS.DoH != nil {
		return s.cfg.DNS.DoH.Timeout.Value(10 * time.Second)
	}
	return s.cfg.ConnectTimeout.Value(10 * time.Second)
}

func (s *Server) newResolver() *net.Resolver {
	if s.cfg.DNS.DoH != nil {
		return s.newDoHResolver(*s.cfg.DNS.DoH)
	}
	if len(s.cfg.DNS.Servers) == 0 {
		// A Dial hook is required even for the resolvers discovered through the
		// host's resolv.conf. A net.DefaultResolver lookup creates its own sockets
		// and therefore does not inherit the SO_BINDTODEVICE setting from the
		// eventual target connection.
		return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := s.dialerWithoutResolver().DialContext(ctx, network, address)
			if err != nil {
				return nil, fmt.Errorf("dial system DNS %s through interface %s: %w", address, s.cfg.Interface, err)
			}
			return conn, nil
		}}
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := s.dialerWithoutResolver()
		var last error
		for _, addr := range s.cfg.DNS.Servers {
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}}
}

func (s *Server) dialerWithoutResolver() *net.Dialer {
	return DeviceDialer(s.cfg.Interface, s.cfg.ConnectTimeout.Value(10*time.Second))
}

func (s *Server) handleConnect(client net.Conn, dst address) error {
	upstream, err := s.DialContext(s.ctx, "tcp", dst.String())
	if err != nil {
		if errors.Is(err, policy.ErrTargetDenied) {
			s.metrics.targetDenied.Add(1)
		}
		_ = writeReply(client, replyForError(err), nil)
		return fmt.Errorf("connect %s: %w", dst.String(), err)
	}
	defer upstream.Close()
	if !s.trackOutbound(upstream) {
		return net.ErrClosed
	}
	defer s.track(upstream, false)
	if err := writeReply(client, repSuccess, upstream.LocalAddr()); err != nil {
		return err
	}
	s.log.Debug("CONNECT", "client", client.RemoteAddr(), "destination", dst.String())
	return s.relayTCP(client, upstream, s.cfg.IdleTimeout.Value(5*time.Minute))
}

func (s *Server) relayTCP(a, b net.Conn, idle time.Duration) error {
	a = &deadlineConn{Conn: a, idle: idle}
	b = &deadlineConn{Conn: b, idle: idle}
	errCh := make(chan error, 2)
	copyOne := func(dst, src net.Conn, counter *atomic.Uint64) {
		_, err := io.Copy(&atomicCountingWriter{Writer: dst, counter: counter}, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		errCh <- err
	}
	go copyOne(a, b, &s.metrics.tcpDownloadBytes)
	go copyOne(b, a, &s.metrics.tcpUploadBytes)
	first := <-errCh
	if first != nil {
		_ = a.Close()
		_ = b.Close()
	}
	second := <-errCh
	_ = a.Close()
	_ = b.Close()
	if first != nil {
		return first
	}
	return second
}

// atomicCountingWriter records bytes after every successful write so live
// metrics move while a stream is active instead of jumping when io.Copy ends.
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
	if x, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return x.CloseWrite()
	}
	return nil
}

func replyForError(err error) byte {
	if errors.Is(err, policy.ErrTargetDenied) {
		return repNotAllowed
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return repConnectionRefused
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return repNetworkUnreachable
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return repHostUnreachable
	}
	if errors.Is(err, os.ErrPermission) {
		return repNotAllowed
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return repTTLExpired
	}
	return repGeneralFailure
}

func isNormalClose(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}
