package socks5

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"wwan-proxy/internal/config"
)

var (
	errAddressType   = errors.New("address type not supported")
	errFragmentedUDP = errors.New("fragmented UDP is not supported")
)

type Server struct {
	cfg      config.Server
	log      *slog.Logger
	listener net.Listener
	resolver *net.Resolver
	// resolverClose releases persistent DoH connections during hot reload and shutdown.
	resolverClose func()

	mu      sync.Mutex
	active  map[net.Conn]struct{}
	closing bool
	wg      sync.WaitGroup
	sem     chan struct{}
	metrics metricCounters
}

func New(cfg config.Server, logger *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: logger.With("server", cfg.Name, "interface", cfg.Interface), active: make(map[net.Conn]struct{})}
	if cfg.MaxConnections > 0 {
		s.sem = make(chan struct{}, cfg.MaxConnections)
	}
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
	if s.closing {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.mu.Unlock()
	s.log.Info("SOCKS5 listening", "address", ln.Addr())
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
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
		if s.sem != nil {
			select {
			case s.sem <- struct{}{}:
			default:
				s.log.Warn("connection limit reached", "remote", conn.RemoteAddr())
				_ = conn.Close()
				continue
			}
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.active[conn] = struct{}{}
		s.wg.Add(1)
		s.metrics.totalConnections.Add(1)
		s.metrics.activeConnections.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer s.metrics.activeConnections.Add(-1)
			defer conn.Close()
			defer s.track(conn, false)
			if s.sem != nil {
				defer func() { <-s.sem }()
			}
			if err := s.handle(conn); err != nil && !isNormalClose(err) {
				s.metrics.connectionErrors.Add(1)
				s.log.Warn("SOCKS5 connection ended with error", "remote", conn.RemoteAddr(), "error", err)
			}
		}()
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closing = true
	ln := s.listener
	for c := range s.active {
		_ = c.Close()
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if s.resolverClose != nil {
		s.resolverClose()
	}
	s.wg.Wait()
	return nil
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
		s.metrics.bindCommands.Add(1)
		return s.handleBind(c, dst)
	case cmdUDPAssociate:
		if !s.cfg.UDP.Enabled {
			_ = writeReply(c, repCommandNotSupported, nil)
			return fmt.Errorf("UDP ASSOCIATE disabled")
		}
		s.metrics.udpAssociations.Add(1)
		return s.handleUDP(c, dst)
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
	expected, ok := s.cfg.Auth.Users[string(u)]
	valid := ok && subtle.ConstantTimeCompare([]byte(expected), p) == 1
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

func (s *Server) resolutionTimeout() time.Duration {
	if s.cfg.DNS.DoH != nil {
		return s.cfg.DNS.DoH.Timeout.Value(10 * time.Second)
	}
	return s.cfg.ConnectTimeout.Value(10 * time.Second)
}

// OutboundDialer returns a dialer that uses the instance resolver and binds
// every resulting socket to the configured network interface.
func (s *Server) OutboundDialer() *net.Dialer { return s.dialer() }

func (s *Server) newResolver() *net.Resolver {
	if s.cfg.DNS.DoH != nil {
		return s.newDoHResolver(*s.cfg.DNS.DoH)
	}
	if len(s.cfg.DNS.Servers) == 0 {
		return net.DefaultResolver
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
	upstream, err := s.dialer().Dial("tcp", dst.String())
	if err != nil {
		_ = writeReply(client, replyForError(err), nil)
		return fmt.Errorf("connect %s: %w", dst.String(), err)
	}
	defer upstream.Close()
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
		n, err := io.Copy(dst, src)
		counter.Add(uint64(n))
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
