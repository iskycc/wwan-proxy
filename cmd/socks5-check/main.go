// socks5-check performs a comprehensive SOCKS5 connectivity test.
// It supports TCP CONNECT, BIND, and UDP ASSOCIATE with username/password auth.
//
// Usage:
//
//	go run ./cmd/socks5-check -proxy 23.141.204.202:59999 -user test -pass test123
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	proxy := flag.String("proxy", "127.0.0.1:1080", "SOCKS5 proxy address")
	user := flag.String("user", "", "username (empty for none)")
	pass := flag.String("pass", "", "password")
	tcpTarget := flag.String("tcp-target", "httpbin.org:80", "TCP CONNECT target")
	udpTarget := flag.String("udp-target", "8.8.8.8:53", "UDP ASSOCIATE target")
	flag.Parse()

	if *user == "" {
		fmt.Println("WARNING: no username provided; will try no-auth if server allows it")
	}

	ok := true

	fmt.Println("=== 1. TCP CONNECT test ===")
	if err := testTCPConnect(*proxy, *user, *pass, *tcpTarget); err != nil {
		fmt.Printf("TCP CONNECT FAILED: %v\n", err)
		ok = false
	} else {
		fmt.Println("TCP CONNECT OK")
	}

	fmt.Println("\n=== 2. BIND test ===")
	if err := testBind(*proxy, *user, *pass); err != nil {
		fmt.Printf("BIND FAILED: %v\n", err)
		ok = false
	} else {
		fmt.Println("BIND OK")
	}

	fmt.Println("\n=== 3. UDP ASSOCIATE test ===")
	if err := testUDPAssociate(*proxy, *user, *pass, *udpTarget); err != nil {
		fmt.Printf("UDP ASSOCIATE FAILED: %v\n", err)
		ok = false
	} else {
		fmt.Println("UDP ASSOCIATE OK")
	}

	if !ok {
		os.Exit(1)
	}
}

func dialSOCKS5(proxy, user, pass string) (net.Conn, error) {
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		return nil, err
	}

	// Greet
	var methods []byte
	if user != "" {
		methods = []byte{2, 0, 2} // none, username/password
	} else {
		methods = []byte{1, 0}
	}
	if _, err := conn.Write(append([]byte{5}, methods...)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("greet write: %w", err)
	}
	var greet [2]byte
	if _, err := io.ReadFull(conn, greet[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("greet read: %w", err)
	}
	if greet[0] != 5 {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected version %d", greet[0])
	}

	switch greet[1] {
	case 0:
		return conn, nil
	case 2:
		if user == "" {
			_ = conn.Close()
			return nil, fmt.Errorf("server required auth but no credentials provided")
		}
		if err := authenticate(conn, user, pass); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("auth method rejected: %d", greet[1])
	}
}

func authenticate(conn net.Conn, user, pass string) error {
	auth := append([]byte{1, byte(len(user))}, []byte(user)...)
	auth = append(auth, byte(len(pass)))
	auth = append(auth, []byte(pass)...)
	if _, err := conn.Write(auth); err != nil {
		return fmt.Errorf("auth write: %w", err)
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return fmt.Errorf("auth read: %w", err)
	}
	if resp[0] != 1 || resp[1] != 0 {
		return fmt.Errorf("auth failed: %v", resp)
	}
	return nil
}

func testTCPConnect(proxy, user, pass, target string) error {
	conn, err := dialSOCKS5(proxy, user, pass)
	if err != nil {
		return err
	}
	defer conn.Close()

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return err
	}

	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("lookup %s: %w", host, err)
	}
	ip := ips[0].To4()
	if ip == nil {
		ip = ips[0]
	}

	if _, err := conn.Write(buildRequest(0x01, ip, port)); err != nil {
		return err
	}
	addr, err := readReply(conn)
	if err != nil {
		return err
	}
	fmt.Printf("  bound relay: %s\n", addr.String())

	req := fmt.Sprintf("GET /get HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no response")
	}
	fmt.Printf("  response: %s...\n", string(buf[:min(n, 80)]))
	return nil
}

func testBind(proxy, user, pass string) error {
	conn, err := dialSOCKS5(proxy, user, pass)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.Write(buildRequest(0x02, net.IPv4zero, 0)); err != nil {
		return err
	}
	bound, err := readReply(conn)
	if err != nil {
		return err
	}
	fmt.Printf("  first reply (bound): %s\n", bound.String())

	// Try to connect as peer from this machine. This may fail if the peer port
	// is not reachable from here; the important part is the first reply.
	go func() {
		time.Sleep(500 * time.Millisecond)
		c, err := net.DialTimeout("tcp", bound.String(), 5*time.Second)
		if err != nil {
			fmt.Printf("  peer dial (informational): %v\n", err)
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("peer-hello"))
	}()

	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	peer, err := readReply(conn)
	if err != nil {
		return err
	}
	fmt.Printf("  second reply (peer): %s\n", peer.String())
	return nil
}

func testUDPAssociate(proxy, user, pass, target string) error {
	conn, err := dialSOCKS5(proxy, user, pass)
	if err != nil {
		return err
	}
	defer conn.Close()

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	port, err := net.LookupPort("udp", portStr)
	if err != nil {
		return err
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("lookup %s: %w", host, err)
	}
	ip := ips[0].To4()
	if ip == nil {
		ip = ips[0]
	}

	// RFC 1928: DST.ADDR in UDP ASSOCIATE should be the client's UDP endpoint.
	// 0.0.0.0:0 means "any" and is the most compatible choice.
	if _, err := conn.Write(buildRequest(0x03, net.IPv4zero, 0)); err != nil {
		return err
	}
	relay, err := readReply(conn)
	if err != nil {
		return err
	}
	fmt.Printf("  relay address: %s\n", relay.String())

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return err
	}
	defer udpConn.Close()

	query := buildDNSQuery(host)
	packet := appendAddress([]byte{0, 0, 0}, ip, port)
	packet = append(packet, query...)
	if _, err := udpConn.WriteToUDP(packet, relay); err != nil {
		return err
	}

	_ = udpConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := udpConn.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	from, payload, err := parseUDPDatagram(buf[:n])
	if err != nil {
		return err
	}
	fmt.Printf("  reply from %s, payload len=%d, flags=%x%x\n", from.String(), len(payload), payload[2], payload[3])
	return nil
}

func buildRequest(cmd byte, ip net.IP, port int) []byte {
	return appendAddress([]byte{5, cmd, 0}, ip, port)
}

func appendAddress(b []byte, ip net.IP, port int) []byte {
	if ip4 := ip.To4(); ip4 != nil {
		b = append(b, 1)
		b = append(b, ip4...)
	} else {
		b = append(b, 4)
		b = append(b, ip.To16()...)
	}
	b = append(b, byte(port>>8), byte(port))
	return b
}

func readReply(conn net.Conn) (*net.UDPAddr, error) {
	var h [3]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return nil, err
	}
	fmt.Printf("  reply header: ver=%d rep=%d rsv=%d\n", h[0], h[1], h[2])
	if h[0] != 5 {
		return nil, fmt.Errorf("unexpected version %d", h[0])
	}
	if h[1] != 0 {
		return nil, fmt.Errorf("SOCKS5 error code %d", h[1])
	}
	addr, err := readAddress(conn)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: net.ParseIP(addr.Host), Port: int(addr.Port)}, nil
}

type address struct {
	Host string
	Port uint16
}

func readAddress(r io.Reader) (address, error) {
	var atyp [1]byte
	if _, err := io.ReadFull(r, atyp[:]); err != nil {
		return address{}, err
	}
	var host string
	switch atyp[0] {
	case 1:
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return address{}, err
		}
		host = net.IP(ip[:]).String()
	case 4:
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return address{}, err
		}
		host = net.IP(ip[:]).String()
	case 3:
		var lenBuf [1]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return address{}, err
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(r, domain); err != nil {
			return address{}, err
		}
		host = string(domain)
	default:
		return address{}, fmt.Errorf("unknown atyp %d", atyp[0])
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(r, portBuf[:]); err != nil {
		return address{}, err
	}
	return address{Host: host, Port: binary.BigEndian.Uint16(portBuf[:])}, nil
}

func parseUDPDatagram(b []byte) (*net.UDPAddr, []byte, error) {
	if len(b) < 10 {
		return nil, nil, fmt.Errorf("datagram too short")
	}
	if b[0] != 0 || b[1] != 0 || b[2] != 0 {
		return nil, nil, fmt.Errorf("invalid udp datagram header %v", b[:3])
	}
	addr, n, err := readAddressFromBytes(b[3:])
	if err != nil {
		return nil, nil, err
	}
	return &net.UDPAddr{IP: net.ParseIP(addr.Host), Port: int(addr.Port)}, b[3+n:], nil
}

func readAddressFromBytes(b []byte) (address, int, error) {
	if len(b) < 1 {
		return address{}, 0, fmt.Errorf("no atyp")
	}
	switch b[0] {
	case 1:
		if len(b) < 7 {
			return address{}, 0, fmt.Errorf("ipv4 too short")
		}
		return address{Host: net.IP(b[1:5]).String(), Port: binary.BigEndian.Uint16(b[5:7])}, 7, nil
	case 4:
		if len(b) < 19 {
			return address{}, 0, fmt.Errorf("ipv6 too short")
		}
		return address{Host: net.IP(b[1:17]).String(), Port: binary.BigEndian.Uint16(b[17:19])}, 19, nil
	case 3:
		if len(b) < 2 {
			return address{}, 0, fmt.Errorf("domain len missing")
		}
		l := int(b[1])
		if len(b) < 2+l+2 {
			return address{}, 0, fmt.Errorf("domain too short")
		}
		return address{Host: string(b[2 : 2+l]), Port: binary.BigEndian.Uint16(b[2+l : 2+l+2])}, 2 + l + 2, nil
	default:
		return address{}, 0, fmt.Errorf("unknown atyp %d", b[0])
	}
}

func buildDNSQuery(name string) []byte {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for _, label := range []string{"example", "com"} {
		q = append(q, byte(len(label)))
		q = append(q, []byte(label)...)
	}
	q = append(q, 0)
	q = append(q, 0x00, 0x01, 0x00, 0x01) // A, IN
	_ = name
	return q
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
