package socks5

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wwan-proxy/internal/config"
)

type dohResolver struct {
	endpoint  string
	upstreams []dohUpstream
	headers   map[string]string
	timeout   time.Duration
	next      atomic.Uint64
	context   context.Context
	cancel    context.CancelFunc
}

type dohUpstream struct {
	bootstrap string
	client    *http.Client
	transport *http.Transport
}

func (s *Server) newDoHResolver(cfg config.DoH) *net.Resolver {
	u, _ := url.Parse(cfg.URL)
	bootstrap := append([]string(nil), cfg.BootstrapIPs...)
	if len(bootstrap) == 0 {
		bootstrap = []string{u.Hostname()}
	}
	resolverContext, resolverCancel := context.WithCancel(context.Background())
	doh := &dohResolver{
		endpoint: cfg.URL,
		headers:  cfg.Headers,
		timeout:  cfg.Timeout.Value(10 * time.Second),
		context:  resolverContext,
		cancel:   resolverCancel,
	}
	for _, bootstrapIP := range bootstrap {
		ip := bootstrapIP
		transport := &http.Transport{
			Proxy:                 nil,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			MaxConnsPerHost:       64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   min(doh.timeout, 10*time.Second),
			ResponseHeaderTimeout: doh.timeout,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				ServerName:         u.Hostname(),
				InsecureSkipVerify: cfg.InsecureSkipVerify, // explicitly configured for private DoH deployments
			},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				return s.dialerWithoutResolver().DialContext(ctx, network, net.JoinHostPort(ip, port))
			},
		}
		client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		doh.upstreams = append(doh.upstreams, dohUpstream{bootstrap: ip, client: client, transport: transport})
	}
	s.resolverClose = func() {
		doh.cancel()
		for _, upstream := range doh.upstreams {
			upstream.transport.CloseIdleConnections()
		}
	}
	return &net.Resolver{PreferGo: true, StrictErrors: false, Dial: func(lookupContext context.Context, network, _ string) (net.Conn, error) {
		connContext, connCancel := context.WithCancel(doh.context)
		conn := &dohConn{resolver: doh, network: network, lookupContext: lookupContext, context: connContext, cancel: connCancel}
		if strings.HasPrefix(network, "udp") {
			return &dohPacketConn{dohConn: conn}, nil
		}
		return conn, nil
	}}
}

func (d *dohResolver) query(ctx context.Context, wire []byte) ([]byte, error) {
	if len(d.upstreams) == 0 {
		return nil, errors.New("no DoH bootstrap address configured")
	}
	start := int((d.next.Add(1) - 1) % uint64(len(d.upstreams)))
	var attemptErrors []error
	for i := range d.upstreams {
		upstream := d.upstreams[(start+i)%len(d.upstreams)]
		attemptContext, cancel := dohAttemptContext(ctx, len(d.upstreams)-i)
		answer, err := d.queryUpstream(attemptContext, upstream, wire)
		cancel()
		if err == nil {
			return answer, nil
		}
		attemptErrors = append(attemptErrors, fmt.Errorf("bootstrap %s: %w", upstream.bootstrap, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("DoH %s failed: %w", d.endpoint, errors.Join(attemptErrors...))
}

func dohAttemptContext(ctx context.Context, remainingAttempts int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || remainingAttempts <= 1 {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, remaining/time.Duration(remainingAttempts))
}

func (d *dohResolver) queryUpstream(ctx context.Context, upstream dohUpstream, wire []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	for key, value := range d.headers {
		req.Header.Set(key, value)
	}
	resp, err := upstream.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("DoH server returned %s", resp.Status)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/dns-message") {
		return nil, fmt.Errorf("DoH server returned invalid Content-Type %q", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, err
	}
	if len(body) < 12 || len(body) > 65535 {
		return nil, fmt.Errorf("invalid DoH DNS response length %d", len(body))
	}
	return body, nil
}

type dohConn struct {
	resolver      *dohResolver
	network       string
	lookupContext context.Context
	context       context.Context
	cancel        context.CancelFunc

	mu            sync.Mutex
	response      []byte
	closed        bool
	readDeadline  time.Time
	writeDeadline time.Time
}

func (c *dohConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.mu.Unlock()

	wire := p
	stream := strings.HasPrefix(c.network, "tcp")
	if stream {
		if len(p) < 2 || int(binary.BigEndian.Uint16(p[:2])) != len(p)-2 {
			return 0, fmt.Errorf("invalid TCP DNS query")
		}
		wire = p[2:]
	}
	// net.Resolver applies the timeout from /etc/resolv.conf to its synthetic
	// DNS connection. That timeout describes UDP/TCP DNS and must not override
	// the explicit DoH timeout. Still propagate real caller cancellation and
	// server shutdown so abandoned HTTPS requests do not linger.
	ctx, cancel := context.WithTimeout(c.context, c.resolver.timeout)
	stopLookupCancel := context.AfterFunc(c.lookupContext, func() {
		if errors.Is(c.lookupContext.Err(), context.Canceled) {
			cancel()
		}
	})
	defer stopLookupCancel()
	defer cancel()
	answer, err := c.resolver.query(ctx, wire)
	if err != nil {
		return 0, err
	}
	if stream {
		framed := make([]byte, 2, len(answer)+2)
		binary.BigEndian.PutUint16(framed, uint16(len(answer)))
		answer = append(framed, answer...)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.response = answer
	return len(p), nil
}

func (c *dohConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if len(c.response) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.response)
	if strings.HasPrefix(c.network, "udp") {
		c.response = nil
	} else {
		c.response = c.response[n:]
	}
	return n, nil
}

func (c *dohConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.response = nil
	c.mu.Unlock()
	c.cancel()
	return nil
}
func (c *dohConn) LocalAddr() net.Addr  { return dohAddr("local") }
func (c *dohConn) RemoteAddr() net.Addr { return dohAddr("doh") }
func (c *dohConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline, c.writeDeadline = t, t
	c.mu.Unlock()
	return nil
}
func (c *dohConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return nil
}
func (c *dohConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return nil
}

type dohAddr string

func (a dohAddr) Network() string { return "doh" }
func (a dohAddr) String() string  { return string(a) }

var _ net.Conn = (*dohConn)(nil)

type dohPacketConn struct{ *dohConn }

func (c *dohPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.RemoteAddr(), err
}
func (c *dohPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.Write(p) }

var _ net.PacketConn = (*dohPacketConn)(nil)
