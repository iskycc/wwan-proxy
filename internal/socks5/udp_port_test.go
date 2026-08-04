package socks5

import (
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestRandomUDPRelayPortsAreInRangeAndOwned(t *testing.T) {
	const count = 64
	conns := make([]*net.UDPConn, 0, count)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	seen := make(map[int]bool)
	for i := 0; i < count; i++ {
		c, err := listenRandomUDP("udp4", net.IPv4(127, 0, 0, 1), 10000, 65535)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		port := c.LocalAddr().(*net.UDPAddr).Port
		if port < 10000 || port > 65535 {
			t.Fatalf("port %d is outside range", port)
		}
		if seen[port] {
			t.Fatalf("port %d was allocated twice", port)
		}
		seen[port] = true
	}
}

func TestFixedUDPRelayPortHasSingleConcurrentOwner(t *testing.T) {
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()

	const contenders = 32
	start := make(chan struct{})
	results := make(chan *net.UDPConn, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			conn, _ := listenUDPRelay("udp4", net.IPv4(127, 0, 0, 1), []int{port}, 10000, 65535)
			results <- conn
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	owners := 0
	for conn := range results {
		if conn == nil {
			continue
		}
		owners++
		_ = conn.Close()
	}
	if owners != 1 {
		t.Fatalf("fixed relay port had %d concurrent owners, want 1", owners)
	}
}

func TestNonContiguousUDPRelayPortPoolHasOneOwnerPerPort(t *testing.T) {
	ports := reserveNonContiguousUDPPorts(t, 3)
	allowed := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		allowed[port] = struct{}{}
	}

	const extraContenders = 16
	start := make(chan struct{})
	type result struct {
		conn *net.UDPConn
		err  error
	}
	results := make(chan result, len(ports)+extraContenders)
	var wg sync.WaitGroup
	for range len(ports) + extraContenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			conn, err := listenUDPRelay("udp4", net.IPv4(127, 0, 0, 1), ports, 10000, 65535)
			results <- result{conn: conn, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var owners []*net.UDPConn
	ownedPorts := make(map[int]struct{}, len(ports))
	for result := range results {
		if result.conn == nil {
			if !errors.Is(result.err, syscall.EADDRINUSE) {
				t.Fatalf("exhausted port pool error=%v, want EADDRINUSE", result.err)
			}
			continue
		}
		owners = append(owners, result.conn)
		port := result.conn.LocalAddr().(*net.UDPAddr).Port
		if _, ok := allowed[port]; !ok {
			t.Fatalf("allocated port %d outside configured pool %v", port, ports)
		}
		if _, duplicate := ownedPorts[port]; duplicate {
			t.Fatalf("configured port %d had multiple concurrent owners", port)
		}
		ownedPorts[port] = struct{}{}
	}
	defer func() {
		for _, conn := range owners {
			_ = conn.Close()
		}
	}()
	if len(owners) != len(ports) {
		t.Fatalf("port pool had %d owners, want capacity %d", len(owners), len(ports))
	}

	releasedPort := owners[0].LocalAddr().(*net.UDPAddr).Port
	_ = owners[0].Close()
	reacquired, err := listenUDPRelay("udp4", net.IPv4(127, 0, 0, 1), ports, 10000, 65535)
	if err != nil {
		t.Fatalf("reacquire released pool port: %v", err)
	}
	defer reacquired.Close()
	if got := reacquired.LocalAddr().(*net.UDPAddr).Port; got != releasedPort {
		t.Fatalf("reacquired port=%d, want the only released port %d", got, releasedPort)
	}
}

func TestFixedUDPRelayPortPoolExhaustionErrorIsBounded(t *testing.T) {
	const poolSize = 64
	reservations := make([]*net.UDPConn, 0, poolSize)
	ports := make([]int, 0, poolSize)
	defer func() {
		for _, conn := range reservations {
			_ = conn.Close()
		}
	}()
	for range poolSize {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		reservations = append(reservations, conn)
		ports = append(ports, conn.LocalAddr().(*net.UDPAddr).Port)
	}

	conn, err := listenFixedUDPPortPool("udp4", net.IPv4(127, 0, 0, 1), ports)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("fully occupied fixed port pool was acquired")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("pool exhaustion error=%v, want EADDRINUSE", err)
	}
	message := err.Error()
	if len(message) > 1024 {
		t.Fatalf("pool exhaustion error grew to %d bytes: %s", len(message), message)
	}
	if got := strings.Count(message, "listen udp4"); got != udpRelayBindErrorSamples {
		t.Fatalf("pool exhaustion error contains %d bind samples, want %d: %s", got, udpRelayBindErrorSamples, message)
	}
	if !strings.Contains(message, "61 additional occupied ports omitted") {
		t.Fatalf("pool exhaustion error lacks omitted-port summary: %s", message)
	}
}

func TestFixedUDPRelayPortPoolStopsOnNonOccupancyError(t *testing.T) {
	ports := []int{12000, 12007, 12019, 12031}
	conn, err := listenFixedUDPPortPool("udp4", net.ParseIP("2001:db8::1"), ports)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("IPv6 bind address unexpectedly worked with udp4")
	}
	if err == nil {
		t.Fatal("incompatible address family unexpectedly returned no error")
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("incompatible address error reported pool exhaustion: %v", err)
	}
	if strings.Contains(err.Error(), "sample bind failures") || !strings.Contains(err.Error(), "candidate 1/4") {
		t.Fatalf("non-occupancy bind error was aggregated instead of returned immediately: %v", err)
	}
}

// reserveNonContiguousUDPPorts holds candidate ports until the complete pool
// is known, then releases them for the test. Keeping adjacent values out makes
// the test exercise a genuinely discrete pool rather than an accidental range.
func reserveNonContiguousUDPPorts(t *testing.T, count int) []int {
	t.Helper()
	var reservations []*net.UDPConn
	defer func() {
		for _, conn := range reservations {
			_ = conn.Close()
		}
	}()
	ports := make([]int, 0, count)
	for attempts := 0; len(ports) < count && attempts < count*100; attempts++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		adjacent := false
		for _, existing := range ports {
			if port >= existing-1 && port <= existing+1 {
				adjacent = true
				break
			}
		}
		if adjacent {
			_ = conn.Close()
			continue
		}
		reservations = append(reservations, conn)
		ports = append(ports, port)
	}
	if len(ports) != count {
		t.Fatalf("could not reserve %d non-contiguous UDP ports", count)
	}
	return ports
}
