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
	bootstrapDNS string
	client       *http.Client
	transport    *http.Transport
}

func (s *Server) newDoHResolver(cfg config.DoH) *net.Resolver {
	u, _ := url.Parse(cfg.URL)
	endpointIP := net.ParseIP(u.Hostname())
	bootstrapDNS := append([]string(nil), cfg.BootstrapIPs...)
	if endpointIP != nil {
		// A literal DoH endpoint needs no bootstrap lookup, but still uses the
		// same request/failover machinery as a named endpoint.
		bootstrapDNS = []string{""}
	}
	resolverContext, resolverCancel := context.WithCancel(context.Background())
	doh := &dohResolver{
		endpoint: cfg.URL,
		headers:  cfg.Headers,
		timeout:  cfg.Timeout.Value(10 * time.Second),
		context:  resolverContext,
		cancel:   resolverCancel,
	}
	s.doh = doh
	for _, configuredDNS := range bootstrapDNS {
		dnsAddress := ""
		label := "direct endpoint"
		var bootstrapResolver *net.Resolver
		if endpointIP == nil {
			dnsAddress, _ = config.NormalizeBootstrapDNSAddress(configuredDNS)
			label = configuredDNS
			bootstrapResolver = &net.Resolver{PreferGo: true, StrictErrors: false, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return s.dialerWithoutResolver().DialContext(ctx, network, dnsAddress)
			}}
		}
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
				ips := []net.IP{endpointIP}
				if endpointIP == nil {
					lookupNetwork := "ip"
					if s.cfg.DNS.IPv4Only {
						lookupNetwork = "ip4"
					}
					ips, err = bootstrapResolver.LookupIP(ctx, lookupNetwork, u.Hostname())
					if err != nil {
						return nil, fmt.Errorf("bootstrap DNS %s resolve %s: %w", configuredDNS, u.Hostname(), err)
					}
				}
				return s.dialDoHEndpoint(ctx, network, port, ips)
			},
		}
		client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		doh.upstreams = append(doh.upstreams, dohUpstream{bootstrapDNS: label, client: client, transport: transport})
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

func (s *Server) dialDoHEndpoint(ctx context.Context, network, port string, ips []net.IP) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, errors.New("bootstrap DNS returned no DoH endpoint address")
	}
	var failures []error
	for i, ip := range ips {
		attemptContext, cancel := dividedAttemptContext(ctx, len(ips)-i)
		dialNetwork := network
		if network == "tcp" {
			if ip.To4() != nil {
				dialNetwork = "tcp4"
			} else {
				dialNetwork = "tcp6"
			}
		}
		conn, err := s.dialerWithoutResolver().DialContext(attemptContext, dialNetwork, net.JoinHostPort(ip.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		failures = append(failures, fmt.Errorf("endpoint %s: %w", ip, err))
	}
	return nil, errors.Join(failures...)
}

func (d *dohResolver) query(ctx context.Context, wire []byte) ([]byte, error) {
	if len(d.upstreams) == 0 {
		return nil, errors.New("no DoH bootstrap DNS server configured")
	}
	start := int((d.next.Add(1) - 1) % uint64(len(d.upstreams)))
	var attemptErrors []error
	for i := range d.upstreams {
		upstream := d.upstreams[(start+i)%len(d.upstreams)]
		attemptContext, cancel := dividedAttemptContext(ctx, len(d.upstreams)-i)
		answer, err := d.queryUpstream(attemptContext, upstream, wire)
		cancel()
		if err == nil {
			return answer, nil
		}
		attemptErrors = append(attemptErrors, fmt.Errorf("bootstrap DNS %s: %w", upstream.bootstrapDNS, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("DoH %s failed: %w", d.endpoint, errors.Join(attemptErrors...))
}

func (d *dohResolver) lookupIPv4(parent context.Context, host string) ([]net.IP, error) {
	query, err := buildDNSAQuery(host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()
	response, err := d.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("resolve IPv4 %s via DoH: %w", host, err)
	}
	ips, err := parseDNSAResponse(response, host)
	if err != nil {
		return nil, fmt.Errorf("resolve IPv4 %s via DoH: %w", host, err)
	}
	return ips, nil
}

func buildDNSAQuery(host string) ([]byte, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" || len(host) > 253 {
		return nil, fmt.Errorf("invalid DNS name %q", host)
	}
	wire := make([]byte, 12, 12+len(host)+6)
	binary.BigEndian.PutUint16(wire[2:4], 0x0100) // Recursion Desired; RFC 8484 recommends an ID of zero.
	binary.BigEndian.PutUint16(wire[4:6], 1)
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid DNS name %q", host)
		}
		wire = append(wire, byte(len(label)))
		wire = append(wire, label...)
	}
	wire = append(wire, 0, 0, 1, 0, 1) // root label, A, IN
	return wire, nil
}

func parseDNSAResponse(wire []byte, host string) ([]net.IP, error) {
	if len(wire) < 12 {
		return nil, io.ErrUnexpectedEOF
	}
	flags := binary.BigEndian.Uint16(wire[2:4])
	if flags&0x8000 == 0 {
		return nil, errors.New("DoH response is not a DNS response")
	}
	if flags&0x0200 != 0 {
		return nil, errors.New("truncated DoH response")
	}
	rcode := flags & 0x000f
	if rcode == 3 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if rcode != 0 {
		return nil, fmt.Errorf("DoH DNS response code %d", rcode)
	}
	offset := 12
	questionCount := int(binary.BigEndian.Uint16(wire[4:6]))
	answerCount := int(binary.BigEndian.Uint16(wire[6:8]))
	for range questionCount {
		var err error
		offset, err = skipDNSName(wire, offset)
		if err != nil || offset+4 > len(wire) {
			return nil, io.ErrUnexpectedEOF
		}
		offset += 4
	}
	result := make([]net.IP, 0, answerCount)
	for range answerCount {
		var err error
		offset, err = skipDNSName(wire, offset)
		if err != nil || offset+10 > len(wire) {
			return nil, io.ErrUnexpectedEOF
		}
		recordType := binary.BigEndian.Uint16(wire[offset : offset+2])
		recordClass := binary.BigEndian.Uint16(wire[offset+2 : offset+4])
		dataLength := int(binary.BigEndian.Uint16(wire[offset+8 : offset+10]))
		offset += 10
		if offset+dataLength > len(wire) {
			return nil, io.ErrUnexpectedEOF
		}
		if recordType == 1 && recordClass == 1 && dataLength == net.IPv4len {
			result = append(result, net.IPv4(wire[offset], wire[offset+1], wire[offset+2], wire[offset+3]))
		}
		offset += dataLength
	}
	if len(result) == 0 {
		return nil, &net.DNSError{Err: "no IPv4 address", Name: host, IsNotFound: true}
	}
	return result, nil
}

func skipDNSName(wire []byte, offset int) (int, error) {
	for labels := 0; labels <= 127; labels++ {
		if offset >= len(wire) {
			return 0, io.ErrUnexpectedEOF
		}
		length := int(wire[offset])
		if length&0xc0 == 0xc0 {
			if offset+2 > len(wire) {
				return 0, io.ErrUnexpectedEOF
			}
			return offset + 2, nil
		}
		if length&0xc0 != 0 {
			return 0, errors.New("invalid compressed DNS name")
		}
		offset++
		if length == 0 {
			return offset, nil
		}
		if offset+length > len(wire) {
			return 0, io.ErrUnexpectedEOF
		}
		offset += length
	}
	return 0, errors.New("too many DNS labels")
}

func dividedAttemptContext(ctx context.Context, remainingAttempts int) (context.Context, context.CancelFunc) {
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
