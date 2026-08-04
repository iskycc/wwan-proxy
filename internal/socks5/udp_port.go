package socks5

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"syscall"
)

// listenUDPRelay atomically acquires either one port from the configured fixed
// pool or one random port from the configured range. net.ListenUDP does not
// enable SO_REUSEPORT, so a successful bind is also the per-association port
// ownership lock, including across listener generations and other processes.
func listenUDPRelay(network string, bindIP net.IP, relayPorts []int, portMin, portMax int) (*net.UDPConn, error) {
	if len(relayPorts) == 0 {
		return listenRandomUDP(network, bindIP, portMin, portMax)
	}
	return listenFixedUDPPortPool(network, bindIP, relayPorts)
}

const (
	udpRelayPortMin          = 10000
	udpRelayPortMax          = 65535
	udpRelayBindErrorSamples = 3
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

// listenFixedUDPPortPool starts at a cryptographically random offset and then
// walks the whole pool. This avoids making the first configured port a hotspot
// while still guaranteeing that an available candidate is tried. The caller's
// slice is never reordered or mutated.
func listenFixedUDPPortPool(network string, bindIP net.IP, relayPorts []int) (*net.UDPConn, error) {
	if len(relayPorts) == 0 {
		return nil, errors.New("configured UDP relay port pool is empty")
	}
	start, err := randomUDPPort(0, len(relayPorts))
	if err != nil {
		return nil, fmt.Errorf("choose UDP relay port pool offset: %w", err)
	}
	bindErrorSamples := make([]string, 0, udpRelayBindErrorSamples)
	for i := 0; i < len(relayPorts); i++ {
		port := relayPorts[(start+i)%len(relayPorts)]
		conn, bindErr := net.ListenUDP(network, &net.UDPAddr{IP: bindIP, Port: port})
		if bindErr == nil {
			return conn, nil
		}
		if !errors.Is(bindErr, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("bind configured UDP relay port %d (candidate %d/%d): %w", port, i+1, len(relayPorts), bindErr)
		}
		if len(bindErrorSamples) < cap(bindErrorSamples) {
			bindErrorSamples = append(bindErrorSamples, fmt.Sprintf("port %d: %v", port, bindErr))
		}
	}
	return nil, &udpRelayPortPoolExhaustedError{
		poolSize: len(relayPorts),
		samples:  bindErrorSamples,
	}
}

// udpRelayPortPoolExhaustedError keeps the diagnostic bounded even for the
// largest accepted pool while retaining errors.Is(err, syscall.EADDRINUSE) for
// SOCKS5 reply mapping and callers that distinguish exhaustion from bad binds.
type udpRelayPortPoolExhaustedError struct {
	poolSize int
	samples  []string
}

func (e *udpRelayPortPoolExhaustedError) Error() string {
	message := fmt.Sprintf("all %d configured UDP relay ports are occupied", e.poolSize)
	if len(e.samples) == 0 {
		return message
	}
	message += "; sample bind failures: " + strings.Join(e.samples, "; ")
	if omitted := e.poolSize - len(e.samples); omitted > 0 {
		message += fmt.Sprintf("; %d additional occupied ports omitted", omitted)
	}
	return message
}

func (*udpRelayPortPoolExhaustedError) Unwrap() error {
	return syscall.EADDRINUSE
}

func randomUDPPort(portMin, count int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(count)))
	if err != nil {
		return 0, err
	}
	return portMin + int(n.Int64()), nil
}
