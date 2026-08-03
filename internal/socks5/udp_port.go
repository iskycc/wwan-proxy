package socks5

import (
	"crypto/rand"
	"errors"
	"math/big"
	"net"
	"syscall"
)

const (
	udpRelayPortMin = 10000
	udpRelayPortMax = 65535
)

// listenRandomUDP chooses a random port and owns it before returning, so another
// process cannot claim the same port between availability checking and binding.
func listenRandomUDP(network string, bindIP net.IP, portMin, portMax int) (*net.UDPConn, error) {
	count := portMax - portMin + 1
	start, err := randomUDPPort(portMin, count)
	if err != nil {
		return nil, err
	}
	for i := 0; i < count; i++ {
		port := portMin + (start-portMin+i)%count
		conn, err := net.ListenUDP(network, &net.UDPAddr{IP: bindIP, Port: port})
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
	}
	return nil, syscall.EADDRINUSE
}

func randomUDPPort(portMin, count int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return 0, err
	}
	return portMin + int(n.Int64()), nil
}
