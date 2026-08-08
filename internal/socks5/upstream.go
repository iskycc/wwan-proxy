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

// UpstreamUDPRelay is one RFC 1928 UDP association with an upstream SOCKS5
// server. The TCP control connection must remain open for as long as Packet is
// used.
type UpstreamUDPRelay struct {
	Control net.Conn
	Packet  *net.UDPConn
	Relay   *net.UDPAddr
}

func (r *UpstreamUDPRelay) Close() error {
	if r == nil {
		return nil
	}
	var packetErr, controlErr error
	if r.Packet != nil {
		packetErr = r.Packet.Close()
	}
	if r.Control != nil {
		controlErr = r.Control.Close()
	}
	return errors.Join(packetErr, controlErr)
}

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

	proxyConn, err := dialUpstreamControl(ctx, upstream, dialer)
	if err != nil {
		return nil, err
	}

	if err := upstreamConnect(proxyConn, host, port); err != nil {
		_ = proxyConn.Close()
		return nil, err
	}

	_ = proxyConn.SetDeadline(time.Time{})
	return proxyConn, nil
}

// OpenUDPViaUpstream establishes an upstream UDP ASSOCIATE and a route-bound
// UDP socket used to exchange encapsulated datagrams with the returned relay.
func OpenUDPViaUpstream(ctx context.Context, upstream config.Upstream, dialer *net.Dialer, resolver *net.Resolver) (*UpstreamUDPRelay, error) {
	if !upstream.Enabled {
		return nil, errors.New("upstream is not enabled")
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	control, err := dialUpstreamControl(ctx, upstream, dialer)
	if err != nil {
		return nil, err
	}
	cleanup := func(packet *net.UDPConn) {
		if packet != nil {
			_ = packet.Close()
		}
		_ = control.Close()
	}
	_, localOK := control.LocalAddr().(*net.TCPAddr)
	remoteTCP, remoteOK := control.RemoteAddr().(*net.TCPAddr)
	if !localOK || !remoteOK {
		cleanup(nil)
		return nil, fmt.Errorf("upstream UDP requires TCP addresses, local=%T remote=%T", control.LocalAddr(), control.RemoteAddr())
	}
	ipv4 := remoteTCP.IP.To4() != nil
	network, listenAddress := "udp6", "[::]:0"
	if ipv4 {
		network, listenAddress = "udp4", "0.0.0.0:0"
	}
	lc := net.ListenConfig{Control: dialer.Control}
	packetConn, err := lc.ListenPacket(ctx, network, listenAddress)
	if err != nil {
		cleanup(nil)
		return nil, fmt.Errorf("open upstream UDP socket: %w", err)
	}
	packet, ok := packetConn.(*net.UDPConn)
	if !ok {
		_ = packetConn.Close()
		cleanup(nil)
		return nil, fmt.Errorf("unexpected upstream UDP socket %T", packetConn)
	}
	// Wildcard source is the most interoperable request across NAT. The actual
	// UDP socket was already created and the upstream pins its observed source
	// endpoint when the first encapsulated datagram arrives.
	requestIP := net.IPv6unspecified
	if ipv4 {
		requestIP = net.IPv4zero
	}
	bound, err := upstreamCommand(control, cmdUDPAssociate, requestIP.String(), 0)
	if err != nil {
		cleanup(packet)
		return nil, err
	}
	relay, err := resolveUpstreamRelay(ctx, bound, remoteTCP.IP, ipv4, resolver)
	if err != nil {
		cleanup(packet)
		return nil, err
	}
	_ = control.SetDeadline(time.Time{})
	return &UpstreamUDPRelay{Control: control, Packet: packet, Relay: relay}, nil
}

func dialUpstreamControl(ctx context.Context, upstream config.Upstream, dialer *net.Dialer) (net.Conn, error) {
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
	_, err := upstreamCommand(conn, cmdConnect, host, port)
	return err
}

func upstreamCommand(conn net.Conn, command byte, host string, port int) (address, error) {
	atyp, addrBytes, err := encodeUpstreamTarget(host)
	if err != nil {
		return address{}, err
	}
	req := make([]byte, 0, 4+len(addrBytes)+2)
	req = append(req, version5, command, 0x00, atyp)
	req = append(req, addrBytes...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return address{}, fmt.Errorf("upstream command %d request: %w", command, err)
	}

	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return address{}, fmt.Errorf("upstream command %d read: %w", command, err)
	}
	if header[0] != version5 {
		return address{}, fmt.Errorf("upstream command %d version mismatch: %d", command, header[0])
	}
	if header[1] != repSuccess {
		return address{}, fmt.Errorf("upstream command %d failed: %s", command, replyString(header[1]))
	}
	bound, err := readUpstreamBoundAddress(conn, header[3])
	if err != nil {
		return address{}, fmt.Errorf("upstream command %d bind address: %w", command, err)
	}
	return bound, nil
}

func readUpstreamBoundAddress(conn net.Conn, atyp byte) (address, error) {
	var host string
	switch atyp {
	case atypIPv4:
		var raw [4]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return address{}, err
		}
		host = net.IP(raw[:]).String()
	case atypDomain:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return address{}, err
		}
		raw := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return address{}, err
		}
		host = string(raw)
	case atypIPv6:
		var raw [16]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return address{}, err
		}
		host = net.IP(raw[:]).String()
	default:
		return address{}, fmt.Errorf("unsupported address type: %d", atyp)
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(conn, portBytes[:]); err != nil {
		return address{}, err
	}
	return address{Host: host, Port: uint16(portBytes[0])<<8 | uint16(portBytes[1])}, nil
}

func resolveUpstreamRelay(ctx context.Context, bound address, controlRemote net.IP, ipv4 bool, resolver *net.Resolver) (*net.UDPAddr, error) {
	if bound.Port == 0 {
		return nil, errors.New("upstream UDP relay returned port zero")
	}
	if ip := net.ParseIP(bound.Host); ip != nil {
		if ip.IsUnspecified() {
			ip = controlRemote
		}
		if ip == nil || (ip.To4() != nil) != ipv4 {
			return nil, fmt.Errorf("upstream UDP relay address family mismatch: %s", bound.String())
		}
		return &net.UDPAddr{IP: ip, Port: int(bound.Port)}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, bound.Host)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream UDP relay %q: %w", bound.Host, err)
	}
	for _, candidate := range ips {
		if (candidate.IP.To4() != nil) == ipv4 {
			return &net.UDPAddr{IP: candidate.IP, Zone: candidate.Zone, Port: int(bound.Port)}, nil
		}
	}
	return nil, fmt.Errorf("upstream UDP relay %q has no matching address family", bound.Host)
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
