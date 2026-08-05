package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"wwan-proxy/internal/config"
)

// DialViaUpstream connects to targetAddress through an upstream SOCKS5 proxy.
// The returned connection is ready for application data after a successful
// CONNECT reply. The caller is responsible for closing the connection.
func DialViaUpstream(ctx context.Context, upstream config.Upstream, dialer *net.Dialer, network, targetAddress string) (net.Conn, error) {
	if !upstream.Enabled {
		return nil, errors.New("upstream is not enabled")
	}

	host, portStr, err := net.SplitHostPort(targetAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid target address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid target port: %s", portStr)
	}

	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second}
	}

	proxyConn, err := dialer.DialContext(ctx, "tcp", upstream.Address)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s: %w", upstream.Address, err)
	}

	deadline := time.Time{}
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	} else if dialer.Timeout > 0 {
		deadline = time.Now().Add(dialer.Timeout)
	}
	if !deadline.IsZero() {
		_ = proxyConn.SetDeadline(deadline)
	}

	if err := upstreamGreeting(proxyConn, upstream.AuthMethod); err != nil {
		_ = proxyConn.Close()
		return nil, err
	}

	if upstream.AuthMethod == "username_password" {
		if err := upstreamPasswordAuth(proxyConn, upstream.Username, upstream.Password); err != nil {
			_ = proxyConn.Close()
			return nil, err
		}
	}

	if err := upstreamConnect(proxyConn, host, port); err != nil {
		_ = proxyConn.Close()
		return nil, err
	}

	_ = proxyConn.SetDeadline(time.Time{})
	return proxyConn, nil
}

func upstreamGreeting(conn net.Conn, authMethod string) error {
	methods := []byte{methodNone}
	if authMethod == "username_password" {
		methods = []byte{methodPassword}
	}
	req := make([]byte, 0, 2+len(methods))
	req = append(req, version5, byte(len(methods)))
	req = append(req, methods...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("upstream greeting: %w", err)
	}

	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("upstream greeting read: %w", err)
	}
	if resp[0] != version5 {
		return fmt.Errorf("upstream greeting version mismatch: %d", resp[0])
	}
	switch resp[1] {
	case methodNone:
		return nil
	case methodPassword:
		if authMethod == "username_password" {
			return nil
		}
		return fmt.Errorf("upstream selected password auth but none requested")
	case methodReject:
		return fmt.Errorf("upstream rejected all authentication methods")
	default:
		return fmt.Errorf("upstream selected unsupported auth method: %d", resp[1])
	}
}

func upstreamPasswordAuth(conn net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return errors.New("upstream username/password must be <= 255 bytes")
	}
	req := make([]byte, 0, 3+len(username)+len(password))
	req = append(req, 0x01, byte(len(username)))
	req = append(req, username...)
	req = append(req, byte(len(password)))
	req = append(req, password...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("upstream password auth: %w", err)
	}

	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("upstream password auth read: %w", err)
	}
	if resp[0] != 0x01 {
		return fmt.Errorf("upstream password auth version mismatch: %d", resp[0])
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("upstream authentication failed: status %d", resp[1])
	}
	return nil
}

func upstreamConnect(conn net.Conn, host string, port int) error {
	atyp, addrBytes, err := encodeUpstreamTarget(host)
	if err != nil {
		return err
	}
	req := make([]byte, 0, 4+len(addrBytes)+2)
	req = append(req, version5, cmdConnect, 0x00, atyp)
	req = append(req, addrBytes...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("upstream connect request: %w", err)
	}

	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("upstream connect read: %w", err)
	}
	if header[0] != version5 {
		return fmt.Errorf("upstream connect version mismatch: %d", header[0])
	}
	if header[1] != repSuccess {
		return fmt.Errorf("upstream connect failed: %s", replyString(header[1]))
	}

	// Discard BND.ADDR / BND.PORT.
	switch header[3] {
	case atypIPv4:
		var discard [4 + 2]byte
		_, err = io.ReadFull(conn, discard[:])
	case atypDomain:
		var length [1]byte
		if _, err = io.ReadFull(conn, length[:]); err == nil {
			discard := make([]byte, int(length[0])+2)
			_, err = io.ReadFull(conn, discard)
		}
	case atypIPv6:
		var discard [16 + 2]byte
		_, err = io.ReadFull(conn, discard[:])
	default:
		return fmt.Errorf("upstream connect returned unsupported address type: %d", header[3])
	}
	if err != nil {
		return fmt.Errorf("upstream connect discard bind address: %w", err)
	}
	return nil
}

func encodeUpstreamTarget(host string) (atyp byte, addr []byte, err error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return atypIPv4, ip4, nil
		}
		return atypIPv6, ip, nil
	}
	if len(host) > 255 {
		return 0, nil, errors.New("upstream target domain too long")
	}
	return atypDomain, append([]byte{byte(len(host))}, host...), nil
}

func replyString(code byte) string {
	switch code {
	case repSuccess:
		return "success"
	case repGeneralFailure:
		return "general failure"
	case repNotAllowed:
		return "connection not allowed"
	case repNetworkUnreachable:
		return "network unreachable"
	case repHostUnreachable:
		return "host unreachable"
	case repConnectionRefused:
		return "connection refused"
	case repTTLExpired:
		return "TTL expired"
	case repCommandNotSupported:
		return "command not supported"
	case repAddressNotSupported:
		return "address type not supported"
	default:
		return fmt.Sprintf("unknown reply %d", code)
	}
}
