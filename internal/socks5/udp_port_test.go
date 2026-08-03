package socks5

import (
	"net"
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
