package socks5

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// racingDNSConn sends a resolver exchange to every explicitly configured DNS
// server and keeps the first one that replies. A UDP Dial cannot establish
// whether a server is alive, so selecting the first successful Dial makes a
// multi-server configuration silently wait on a dead first server.
type racingDNSConn struct {
	mu        sync.Mutex
	conns     []net.Conn
	closed    bool
	closeOnce sync.Once
}

func newRacingDNSConn(network string, conns []net.Conn) net.Conn {
	conn := &racingDNSConn{conns: conns}
	if strings.HasPrefix(network, "udp") {
		return &racingDNSPacketConn{racingDNSConn: conn}
	}
	return conn
}

// racingDNSPacketConn preserves net.PacketConn's marker interface so Go's DNS
// client uses packet framing for UDP. The supplied address is intentionally
// ignored because every underlying socket is already connected to its server.
type racingDNSPacketConn struct{ *racingDNSConn }

func (c *racingDNSPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.RemoteAddr(), err
}

func (c *racingDNSPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.Write(p)
}

func (c *racingDNSConn) Read(p []byte) (int, error) {
	conns := c.snapshot()
	if len(conns) == 0 {
		return 0, net.ErrClosed
	}
	type result struct {
		conn net.Conn
		data []byte
		n    int
		err  error
	}
	results := make(chan result, len(conns))
	for _, conn := range conns {
		go func() {
			buf := make([]byte, len(p))
			n, err := conn.Read(buf)
			results <- result{conn: conn, data: buf, n: n, err: err}
		}()
	}
	var failures []error
	for range conns {
		outcome := <-results
		if outcome.err == nil {
			c.keep(outcome.conn)
			return copy(p, outcome.data[:outcome.n]), nil
		}
		failures = append(failures, outcome.err)
	}
	return 0, errors.Join(failures...)
}

func (c *racingDNSConn) Write(p []byte) (int, error) {
	conns := c.snapshot()
	if len(conns) == 0 {
		return 0, net.ErrClosed
	}
	var failures []error
	written := false
	for _, conn := range conns {
		n, err := conn.Write(p)
		if err == nil && n == len(p) {
			written = true
			continue
		}
		if err == nil {
			err = errors.New("short DNS write")
		}
		failures = append(failures, err)
	}
	if written {
		return len(p), nil
	}
	return 0, errors.Join(failures...)
}

func (c *racingDNSConn) Close() error {
	var failures []error
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		conns := c.conns
		c.conns = nil
		c.mu.Unlock()
		for _, conn := range conns {
			if err := conn.Close(); err != nil {
				failures = append(failures, err)
			}
		}
	})
	return errors.Join(failures...)
}

func (c *racingDNSConn) LocalAddr() net.Addr  { return c.address(false) }
func (c *racingDNSConn) RemoteAddr() net.Addr { return c.address(true) }

func (c *racingDNSConn) SetDeadline(t time.Time) error {
	return c.setDeadline(func(conn net.Conn) error { return conn.SetDeadline(t) })
}

func (c *racingDNSConn) SetReadDeadline(t time.Time) error {
	return c.setDeadline(func(conn net.Conn) error { return conn.SetReadDeadline(t) })
}

func (c *racingDNSConn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline(func(conn net.Conn) error { return conn.SetWriteDeadline(t) })
}

func (c *racingDNSConn) snapshot() []net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]net.Conn(nil), c.conns...)
}

func (c *racingDNSConn) keep(winner net.Conn) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	losers := make([]net.Conn, 0, len(c.conns)-1)
	for _, conn := range c.conns {
		if conn != winner {
			losers = append(losers, conn)
		}
	}
	c.conns = []net.Conn{winner}
	c.mu.Unlock()
	for _, conn := range losers {
		_ = conn.Close()
	}
}

func (c *racingDNSConn) address(remote bool) net.Addr {
	conns := c.snapshot()
	if len(conns) == 0 {
		return nil
	}
	if remote {
		return conns[0].RemoteAddr()
	}
	return conns[0].LocalAddr()
}

func (c *racingDNSConn) setDeadline(set func(net.Conn) error) error {
	var failures []error
	for _, conn := range c.snapshot() {
		if err := set(conn); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
