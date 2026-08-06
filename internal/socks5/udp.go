package socks5

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"wwan-proxy/internal/policy"
)

const (
	udpReadBufferSize = 65535
	udpSocketBuffer   = 1 << 20
	udpResolveWorkers = 2
	udpResolveQueue   = 8
	udpTargetTTL      = 2 * time.Minute
	udpMaxTargets     = 4096
)

type udpRequest struct {
	dst     address
	payload []byte
}

type udpTargetGrant struct {
	committedUntil time.Time
	pending        map[uint64]time.Time
}

type udpGrantToken struct {
	key netip.AddrPort
	id  uint64
}

type udpWriteFunc func(*net.UDPConn, []byte, *net.UDPAddr) (int, error)

type sourceAdvertiseEntry struct {
	source *net.IPNet
	relay  net.IP
}

type udpAssociation struct {
	server    *Server
	client    *net.UDPConn
	targetTTL time.Duration

	mu             sync.RWMutex
	clientAt       *net.UDPAddr
	out4           *net.UDPConn
	out6           *net.UDPConn
	allowedTargets map[netip.AddrPort]*udpTargetGrant
	nextGrantID    uint64
	writeTarget    udpWriteFunc
	wg             sync.WaitGroup
	lastUnix       atomic.Int64
}

// handleUDP remains as a compatibility wrapper for tests and embedders. The
// server lifecycle should call handleUDPContext so shutdown cancels DNS,
// socket creation, and the association immediately.
func (s *Server) handleUDP(control net.Conn, requested address) error {
	return s.handleUDPContext(context.Background(), control, requested)
}

func (s *Server) handleUDPContext(parent context.Context, control net.Conn, requested address) error {
	localTCP, ok := control.LocalAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("unexpected control local address %T", control.LocalAddr())
	}
	remoteTCP, ok := control.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("unexpected control remote address %T", control.RemoteAddr())
	}
	releaseClient, allowed := s.udpClients.Acquire(control.RemoteAddr())
	if !allowed {
		s.metrics.connectionLimitDrops.Add(1)
		_ = writeReply(control, repNotAllowed, nil)
		return fmt.Errorf("UDP association limit reached for %s", remoteTCP.IP)
	}
	defer releaseClient()
	releaseAssociation, allowed := s.udpLimiter.Acquire()
	if !allowed {
		s.metrics.connectionLimitDrops.Add(1)
		_ = writeReply(control, repNotAllowed, nil)
		return fmt.Errorf("UDP association limit %d reached", s.cfg.UDP.MaxAssociations)
	}
	defer releaseAssociation()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if err := s.validateUDPClient(ctx, requested, remoteTCP.IP); err != nil {
		_ = writeReply(control, repNotAllowed, nil)
		return err
	}

	bindIP := net.ParseIP(s.cfg.UDP.BindIP)
	if bindIP == nil {
		_ = writeReply(control, repGeneralFailure, nil)
		return fmt.Errorf("invalid UDP bind IP %q", s.cfg.UDP.BindIP)
	}
	bindIPv4 := bindIP.To4() != nil
	if bindIPv4 != (remoteTCP.IP.To4() != nil) {
		_ = writeReply(control, repAddressNotSupported, nil)
		return fmt.Errorf("UDP relay bind address family does not match control peer %s", remoteTCP.IP)
	}
	network := "udp6"
	if bindIPv4 {
		network = "udp4"
	}
	portMin, portMax := s.cfg.UDP.PortMin, s.cfg.UDP.PortMax
	if portMin == 0 {
		portMin = udpRelayPortMin
	}
	if portMax == 0 {
		portMax = udpRelayPortMax
	}
	fixedRelayPorts := s.cfg.UDP.FixedRelayPorts()
	clientConn, err := listenUDPRelay(network, bindIP, fixedRelayPorts, portMin, portMax)
	if err != nil {
		_ = writeReply(control, replyForError(err), nil)
		return fmt.Errorf("UDP relay listen: %w", err)
	}
	defer clientConn.Close()
	s.configureUDPBuffers(clientConn, "relay")

	advertiseIP, err := s.udpAdvertiseIP(localTCP.IP, remoteTCP.IP)
	if err != nil {
		_ = writeReply(control, repGeneralFailure, nil)
		return err
	}
	if (advertiseIP.To4() != nil) != bindIPv4 {
		_ = writeReply(control, repAddressNotSupported, nil)
		return fmt.Errorf("UDP relay advertise address family does not match bind address")
	}
	relayPort := clientConn.LocalAddr().(*net.UDPAddr).Port
	if err := writeReply(control, repSuccess, &net.UDPAddr{IP: advertiseIP, Port: relayPort}); err != nil {
		return err
	}

	idle := s.cfg.UDP.IdleTimeout.Value(2 * time.Minute)
	targetTTL := udpTargetTTL
	if idle > targetTTL {
		targetTTL = idle
	}
	assoc := &udpAssociation{
		server:         s,
		client:         clientConn,
		targetTTL:      targetTTL,
		allowedTargets: make(map[netip.AddrPort]*udpTargetGrant),
	}
	assoc.lastUnix.Store(time.Now().UnixNano())
	s.metrics.activeUDP.Add(1)
	defer s.metrics.activeUDP.Add(-1)
	defer assoc.close()
	s.log.Debug("UDP ASSOCIATE", "client", control.RemoteAddr(), "relay", net.JoinHostPort(advertiseIP.String(), fmt.Sprint(relayPort)), "fixed_pool", len(fixedRelayPorts) != 0, "pool_size", len(fixedRelayPorts))

	// RFC 1928 ties the UDP association lifetime to this TCP control
	// connection. Any EOF/error cancels queued DNS and closes the UDP socket.
	go func() {
		var one [1]byte
		_, _ = control.Read(one[:])
		cancel()
	}()
	defer func() { _ = control.SetReadDeadline(time.Now()) }()
	stopClose := context.AfterFunc(ctx, func() { _ = clientConn.Close() })
	defer stopClose()
	return assoc.loop(ctx, idle)
}

func (s *Server) validateUDPClient(parent context.Context, requested address, peerIP net.IP) error {
	requestedIP := net.ParseIP(requested.Host)
	if requestedIP != nil {
		// RFC 1928 says the client MAY put the address/port it expects to use
		// for UDP datagrams in the UDP ASSOCIATE request. In NAT environments
		// (e.g. WiFi Calling, iCloud Private Relay) this advertised address
		// often differs from the TCP control connection's peer IP. We accept
		// any valid unicast address (or the wildcard) here; the actual source
		// is pinned when the first UDP datagram arrives.
		if requestedIP.IsUnspecified() || usableUDPReplyIP(requestedIP) {
			return nil
		}
		return fmt.Errorf("UDP requested client IP %s is not a usable unicast address", requestedIP)
	}
	ctx, cancel := context.WithTimeout(parent, s.resolutionTimeout())
	defer cancel()
	var ips []net.IPAddr
	var err error
	if s.cfg.DNS.IPv4Only {
		resolved, lookupErr := s.lookupIPv4(ctx, requested.Host)
		err = lookupErr
		for _, ip := range resolved {
			ips = append(ips, net.IPAddr{IP: ip})
		}
	} else {
		ips, err = s.resolver.LookupIPAddr(ctx, requested.Host)
	}
	if err != nil {
		return fmt.Errorf("resolve UDP requested client host %q: %w", requested.Host, err)
	}
	for _, ip := range ips {
		if usableUDPReplyIP(ip.IP) {
			return nil
		}
	}
	return fmt.Errorf("UDP requested client host %q did not resolve to a usable unicast address", requested.Host)
}

func (s *Server) udpAdvertiseIP(controlLocal, controlRemote net.IP) (net.IP, error) {
	bindIP, explicitIP, advertiseMap, sourceEntries, err := s.validatedUDPAdvertiseConfig()
	if err != nil {
		return nil, err
	}
	if controlRemote != nil {
		for _, entry := range sourceEntries {
			if entry.source.Contains(controlRemote) {
				return entry.relay, nil
			}
		}
	}
	if controlLocal != nil {
		if mappedIP, ok := advertiseMap[controlLocal.String()]; ok {
			return mappedIP, nil
		}
	}
	if explicitIP != nil {
		return explicitIP, nil
	}
	if !bindIP.IsUnspecified() {
		return bindIP, nil
	}
	if controlLocal == nil || controlLocal.IsUnspecified() {
		return nil, fmt.Errorf("cannot automatically determine UDP relay IP from control connection")
	}
	controlLocal, err = validateUDPReplyIP(controlLocal, fmt.Sprintf("control connection local IP %q", controlLocal))
	if err != nil {
		return nil, err
	}
	if !sameUDPIPFamily(bindIP, controlLocal) {
		return nil, fmt.Errorf("control connection local IP %q does not match UDP bind address family", controlLocal)
	}
	// When the control connection local address is not externally reachable
	// (e.g. a cloud VM behind 1:1 NAT or a container with only a private IP),
	// advertising it to an external SOCKS5 client would make the UDP relay
	// unusable. Try to find a public address on a local interface first.
	if !isPublicUnicastIP(controlLocal) {
		publicIP, findErr := publicInterfaceIPFunc(bindIP.To4() != nil)
		if findErr == nil {
			return publicIP, nil
		}
		// No public interface address was found. On internal or multi-homed
		// servers the client is on the same network as the SOCKS5 listener,
		// so advertising the control connection's local address lets the
		// client send UDP datagrams to the interface it already reached.
		s.log.Debug("UDP relay falling back to control-local address", "control_local", controlLocal)
		return controlLocal, nil
	}
	return controlLocal, nil
}

// validatedUDPAdvertiseConfig validates the complete runtime view before any
// precedence rule is applied. This is intentionally stricter than validating
// only the selected candidate: legacy SQLite rows can otherwise hide bad
// explicit values or non-matching map entries behind a valid mapping.
func (s *Server) validatedUDPAdvertiseConfig() (net.IP, net.IP, map[string]net.IP, []sourceAdvertiseEntry, error) {
	bindIP := net.ParseIP(s.cfg.UDP.BindIP)
	if !usableUDPBindIP(bindIP) {
		return nil, nil, nil, nil, fmt.Errorf("UDP bind IP %q is not unspecified or a usable unicast address", s.cfg.UDP.BindIP)
	}

	var explicitIP net.IP
	if s.cfg.UDP.Advertise != "auto" {
		var err error
		explicitIP, err = validateUDPReplyIP(net.ParseIP(s.cfg.UDP.Advertise), fmt.Sprintf("UDP advertise IP %q", s.cfg.UDP.Advertise))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if !sameUDPIPFamily(bindIP, explicitIP) {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise IP %q does not match UDP bind address family", s.cfg.UDP.Advertise)
		}
	}

	advertiseMap := make(map[string]net.IP, len(s.cfg.UDP.AdvertiseMap))
	for local, relay := range s.cfg.UDP.AdvertiseMap {
		localIP, relayIP := net.ParseIP(local), net.ParseIP(relay)
		if !usableUDPAdvertiseMapLocalIP(localIP) {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise map local IP %q is not a usable unicast address", local)
		}
		if !usableUDPReplyIP(relayIP) {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise map relay IP %q is not a usable SOCKS5 UDP relay address", relay)
		}
		if !sameUDPIPFamily(bindIP, localIP) || !sameUDPIPFamily(bindIP, relayIP) {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise map entry %q -> %q does not match UDP bind address family", local, relay)
		}
		canonicalLocal := localIP.String()
		if _, duplicate := advertiseMap[canonicalLocal]; duplicate {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise map contains duplicate normalized local IP %q", canonicalLocal)
		}
		advertiseMap[canonicalLocal] = relayIP
	}

	sourceEntries := make([]sourceAdvertiseEntry, 0, len(s.cfg.UDP.AdvertiseSourceMap))
	for source, relay := range s.cfg.UDP.AdvertiseSourceMap {
		var sourceNet *net.IPNet
		if ip := net.ParseIP(source); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			sourceNet = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		} else {
			_, parsed, err := net.ParseCIDR(source)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("UDP advertise source map key %q: %w", source, err)
			}
			sourceNet = parsed
		}
		relayIP := net.ParseIP(relay)
		if !usableUDPReplyIP(relayIP) {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise source map relay IP %q is not a usable SOCKS5 UDP relay address", relay)
		}
		if !sameUDPIPFamily(bindIP, sourceNet.IP) || !sameUDPIPFamily(bindIP, relayIP) {
			return nil, nil, nil, nil, fmt.Errorf("UDP advertise source map entry %q -> %q does not match UDP bind address family", source, relay)
		}
		sourceEntries = append(sourceEntries, sourceAdvertiseEntry{source: sourceNet, relay: relayIP})
	}
	return bindIP, explicitIP, advertiseMap, sourceEntries, nil
}

// usableUDPReplyIP prevents legacy or otherwise unvalidated configuration from
// putting an unusable address in a successful SOCKS5 UDP ASSOCIATE reply.
// IPv4 link-local addresses are retained for interface-bound deployments, while
// IPv6 link-local addresses are rejected because SOCKS5 cannot carry a zone ID.
func usableUDPReplyIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsGlobalUnicast() || ip.IsLoopback() {
		return true
	}
	return ip.To4() != nil && ip.IsLinkLocalUnicast()
}

func usableUDPBindIP(ip net.IP) bool {
	return ip != nil && (ip.IsUnspecified() || usableUDPReplyIP(ip))
}

func usableUDPAdvertiseMapLocalIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && (ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

func sameUDPIPFamily(a, b net.IP) bool {
	return a != nil && b != nil && (a.To4() != nil) == (b.To4() != nil)
}

func validateUDPReplyIP(ip net.IP, source string) (net.IP, error) {
	if !usableUDPReplyIP(ip) {
		return nil, fmt.Errorf("%s is not a usable SOCKS5 UDP relay address", source)
	}
	return ip, nil
}

// publicInterfaceIPFunc is the implementation used by udpAdvertiseIP to
// discover a public interface address. It is a package variable so tests can
// substitute a deterministic stub.
var publicInterfaceIPFunc = publicInterfaceIP

// isPublicUnicastIP reports whether ip is a globally routable unicast address
// that an external SOCKS5 client can use to reach this relay. It rejects
// unspecified, loopback, link-local, multicast, RFC 1918 private IPv4 and
// IPv6 unique-local addresses.
func isPublicUnicastIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch ip4[0] {
		case 10:
			return false
		case 172:
			if ip4[1] >= 16 && ip4[1] <= 31 {
				return false
			}
		case 192:
			if ip4[1] == 168 {
				return false
			}
		}
		return true
	}
	// IPv6 unique local addresses (fc00::/7).
	if ip[0]&0xfe == 0xfc {
		return false
	}
	return true
}

// publicInterfaceIP returns a public unicast IP address of the requested
// address family (IPv4 when ipv4 is true, IPv6 otherwise) assigned to a local
// network interface. This is used as a last-resort auto-discovery when the
// control connection local address is not externally reachable.
func publicInterfaceIP(ipv4 bool) (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch a := addr.(type) {
			case *net.IPNet:
				ip = a.IP
			case *net.IPAddr:
				ip = a.IP
			default:
				continue
			}
			if ip == nil {
				continue
			}
			if (ip.To4() != nil) != ipv4 {
				continue
			}
			if !isPublicUnicastIP(ip) {
				continue
			}
			return ip, nil
		}
	}
	family := "IPv4"
	if !ipv4 {
		family = "IPv6"
	}
	return nil, fmt.Errorf("no public %s address found on local interfaces", family)
}

func (a *udpAssociation) loop(parent context.Context, idle time.Duration) error {
	ctx, cancel := context.WithCancel(parent)
	jobs := make(chan udpRequest, udpResolveQueue)
	var workers sync.WaitGroup
	for range udpResolveWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				a.forward(ctx, job)
			}
		}()
	}
	stopClose := context.AfterFunc(ctx, a.closeSockets)
	defer func() {
		cancel()
		close(jobs)
		// Close both directions before waiting. UDP writes have no context-aware
		// API and may be parked in the runtime poller under socket backpressure;
		// closing their socket is what makes cancellation and idle expiry prompt.
		a.closeSockets()
		workers.Wait()
		// A worker that had already passed its context check may have raced the
		// first close while entering outbound. Close once more after every worker
		// has stopped, then wait for response readers.
		a.closeSockets()
		a.wg.Wait()
		stopClose()
	}()

	buf := make([]byte, udpReadBufferSize)
	for {
		_ = a.client.SetReadDeadline(time.Now().Add(time.Second))
		n, _, flags, from, err := a.client.ReadMsgUDP(buf, nil)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				last := time.Unix(0, a.lastUnix.Load())
				if time.Since(last) >= idle {
					return nil
				}
				continue
			}
			return err
		}
		if flags&syscall.MSG_TRUNC != 0 {
			a.server.metrics.udpTruncatedDrops.Add(1)
			continue
		}
		if !a.acceptClient(from) {
			a.server.metrics.udpClientSourceDrops.Add(1)
			continue
		}
		dst, payload, err := parseUDPDatagram(buf[:n])
		if err != nil {
			if errors.Is(err, errFragmentedUDP) {
				a.server.metrics.udpFragmentDrops.Add(1)
			} else {
				a.server.metrics.udpInvalidDrops.Add(1)
			}
			continue
		}
		// There is one producer per association. Check capacity before copying a
		// potentially 64 KiB payload so overload drops do not themselves create a
		// high-rate allocation/GC denial of service.
		if len(jobs) >= cap(jobs) {
			a.server.metrics.udpQueueDrops.Add(1)
			continue
		}
		job := udpRequest{dst: dst, payload: append([]byte(nil), payload...)}
		select {
		case jobs <- job:
		case <-ctx.Done():
			return nil
		default:
			a.server.metrics.udpQueueDrops.Add(1)
		}
	}
}

func (a *udpAssociation) forward(ctx context.Context, job udpRequest) {
	if ctx.Err() != nil {
		return
	}
	targets, err := a.resolve(ctx, job.dst)
	if err != nil {
		if errors.Is(err, policy.ErrTargetDenied) {
			a.server.metrics.targetDenied.Add(1)
		} else if ctx.Err() == nil {
			a.server.metrics.udpResolveDrops.Add(1)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	a.sendToTargets(ctx, job.payload, targets)
}

func (a *udpAssociation) sendToTargets(ctx context.Context, payload []byte, targets []*net.UDPAddr) bool {
	for _, target := range targets {
		if ctx.Err() != nil {
			return false
		}
		out, outboundErr := a.outbound(ctx, target.IP.To4() != nil)
		if outboundErr != nil {
			continue
		}
		grant := a.beginTargetGrant(target)
		_, sendErr := a.writeToTarget(out, payload, target)
		a.finishTargetGrant(grant, sendErr == nil)
		if sendErr != nil {
			continue
		}
		a.server.metrics.udpUploadPackets.Add(1)
		a.server.metrics.udpUploadBytes.Add(uint64(len(payload)))
		a.lastUnix.Store(time.Now().UnixNano())
		return true
	}
	if ctx.Err() == nil {
		a.server.metrics.udpSendErrors.Add(1)
	}
	return false
}

func (a *udpAssociation) writeToTarget(conn *net.UDPConn, payload []byte, target *net.UDPAddr) (int, error) {
	if a.writeTarget != nil {
		return a.writeTarget(conn, payload, target)
	}
	return conn.WriteToUDP(payload, target)
}

func (a *udpAssociation) acceptClient(from *net.UDPAddr) bool {
	if from == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clientAt == nil {
		// Pin the association to the first UDP source we see. This is both
		// more compatible with NAT (where the advertised client IP may differ
		// from the actual UDP source) and secure: an attacker would have to
		// race the legitimate client for the first datagram.
		a.clientAt = cloneUDPAddr(from)
		return true
	}
	return from.IP.Equal(a.clientAt.IP) && from.Port == a.clientAt.Port && from.Zone == a.clientAt.Zone
}

func (a *udpAssociation) resolve(parent context.Context, dst address) ([]*net.UDPAddr, error) {
	if dst.Port == 0 {
		return nil, fmt.Errorf("UDP destination port must not be zero")
	}
	if ip := net.ParseIP(dst.Host); ip != nil {
		return a.selectResolvedTargets(dst, []net.IPAddr{{IP: ip}})
	}
	ctx, cancel := context.WithTimeout(parent, a.server.resolutionTimeout())
	defer cancel()
	var ips []net.IPAddr
	var err error
	if a.server.cfg.DNS.IPv4Only {
		resolved, lookupErr := a.server.lookupIPv4(ctx, dst.Host)
		err = lookupErr
		for _, ip := range resolved {
			ips = append(ips, net.IPAddr{IP: ip})
		}
	} else {
		ips, err = a.server.resolver.LookupIPAddr(ctx, dst.Host)
	}
	if err != nil || len(ips) == 0 {
		if err == nil {
			err = fmt.Errorf("no address")
		}
		return nil, err
	}
	return a.selectResolvedTargets(dst, ips)
}

func (a *udpAssociation) selectResolvedTarget(dst address, ips []net.IPAddr) (*net.UDPAddr, error) {
	targets, err := a.selectResolvedTargets(dst, ips)
	if err != nil {
		return nil, err
	}
	return targets[0], nil
}

func (a *udpAssociation) selectResolvedTargets(dst address, ips []net.IPAddr) ([]*net.UDPAddr, error) {
	targets := make([]*net.UDPAddr, 0, len(ips))
	for _, candidate := range ips {
		if a.server.access == nil || a.server.access.AllowTarget(dst.Host, candidate.IP, int(dst.Port)) {
			targets = append(targets, &net.UDPAddr{IP: candidate.IP, Zone: candidate.Zone, Port: int(dst.Port)})
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: %s resolved only to denied addresses", policy.ErrTargetDenied, dst.String())
	}
	return targets, nil
}

func (a *udpAssociation) outbound(ctx context.Context, ipv4 bool) (*net.UDPConn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot := &a.out6
	network, addr := "udp6", "[::]:0"
	if ipv4 {
		slot, network, addr = &a.out4, "udp4", "0.0.0.0:0"
	}
	if *slot != nil {
		return *slot, nil
	}
	lc := net.ListenConfig{Control: bindToDevice(a.server.cfg.Interface)}
	pc, err := lc.ListenPacket(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("unexpected packet connection %T", pc)
	}
	a.server.configureUDPBuffers(conn, "outbound")
	*slot = conn
	a.wg.Add(1)
	go a.readResponses(conn)
	return conn, nil
}

func (s *Server) configureUDPBuffers(conn *net.UDPConn, role string) {
	if err := conn.SetReadBuffer(udpSocketBuffer); err != nil {
		s.log.Debug("cannot enlarge UDP receive buffer", "role", role, "error", err)
	}
	if err := conn.SetWriteBuffer(udpSocketBuffer); err != nil {
		s.log.Debug("cannot enlarge UDP send buffer", "role", role, "error", err)
	}
}

func (a *udpAssociation) readResponses(conn *net.UDPConn) {
	defer a.wg.Done()
	buf := make([]byte, udpReadBufferSize)
	for {
		n, _, flags, from, err := conn.ReadMsgUDP(buf, nil)
		if err != nil {
			return
		}
		if flags&syscall.MSG_TRUNC != 0 {
			a.server.metrics.udpTruncatedDrops.Add(1)
			continue
		}
		if !a.targetAllowed(from, time.Now()) {
			a.server.metrics.udpResponseSourceDrops.Add(1)
			continue
		}
		a.mu.RLock()
		clientAt := cloneUDPAddr(a.clientAt)
		a.mu.RUnlock()
		if clientAt == nil {
			continue
		}
		packet := makeUDPDatagram(from, buf[:n])
		if _, err := a.client.WriteToUDP(packet, clientAt); err != nil {
			a.server.metrics.udpSendErrors.Add(1)
			continue
		}
		a.server.metrics.udpDownloadPackets.Add(1)
		a.server.metrics.udpDownloadBytes.Add(uint64(n))
		a.lastUnix.Store(time.Now().UnixNano())
	}
}

func (a *udpAssociation) beginTargetGrant(target *net.UDPAddr) udpGrantToken {
	now := time.Now()
	expires := now.Add(a.targetTTL)
	key := a.targetKey(target)
	a.mu.Lock()
	grant := a.allowedTargets[key]
	if grant != nil {
		a.pruneOneTargetGrantLocked(key, grant, now)
		grant = a.allowedTargets[key]
	}
	if grant == nil {
		if len(a.allowedTargets) >= udpMaxTargets {
			// Full-map work is reserved for the capacity boundary. The steady
			// state for an existing target remains O(1), even after a client has
			// contacted thousands of other targets.
			a.pruneTargetGrantsLocked(now)
			if len(a.allowedTargets) >= udpMaxTargets {
				a.evictOldestTargetGrantLocked()
			}
		}
		grant = &udpTargetGrant{pending: make(map[uint64]time.Time)}
		a.allowedTargets[key] = grant
	}
	a.nextGrantID++
	if a.nextGrantID == 0 {
		a.nextGrantID++
	}
	id := a.nextGrantID
	grant.pending[id] = expires
	a.mu.Unlock()
	return udpGrantToken{key: key, id: id}
}

func (a *udpAssociation) finishTargetGrant(token udpGrantToken, success bool) {
	now := time.Now()
	a.mu.Lock()
	grant := a.allowedTargets[token.key]
	if grant != nil {
		expires, exists := grant.pending[token.id]
		delete(grant.pending, token.id)
		if success && exists && expires.After(grant.committedUntil) {
			grant.committedUntil = expires
		}
		a.pruneOneTargetGrantLocked(token.key, grant, now)
	}
	a.mu.Unlock()
}

func (a *udpAssociation) targetAllowed(from *net.UDPAddr, now time.Time) bool {
	key := a.targetKey(from)
	a.mu.Lock()
	grant := a.allowedTargets[key]
	if grant != nil {
		a.pruneOneTargetGrantLocked(key, grant, now)
		grant = a.allowedTargets[key]
	}
	ok := grant != nil && (grant.committedUntil.After(now) || len(grant.pending) > 0)
	a.mu.Unlock()
	return ok
}

func (a *udpAssociation) pruneTargetGrantsLocked(now time.Time) {
	for key, grant := range a.allowedTargets {
		a.pruneOneTargetGrantLocked(key, grant, now)
	}
}

func (a *udpAssociation) pruneOneTargetGrantLocked(key netip.AddrPort, grant *udpTargetGrant, now time.Time) {
	if !grant.committedUntil.After(now) {
		grant.committedUntil = time.Time{}
	}
	for id, expires := range grant.pending {
		if !expires.After(now) {
			delete(grant.pending, id)
		}
	}
	if grant.committedUntil.IsZero() && len(grant.pending) == 0 {
		delete(a.allowedTargets, key)
	}
}

func (a *udpAssociation) evictOldestTargetGrantLocked() {
	var oldestKey netip.AddrPort
	var oldestDeadline time.Time
	for key, grant := range a.allowedTargets {
		deadline := grant.committedUntil
		for _, pending := range grant.pending {
			if pending.After(deadline) {
				deadline = pending
			}
		}
		if oldestDeadline.IsZero() || deadline.Before(oldestDeadline) {
			oldestKey, oldestDeadline = key, deadline
		}
	}
	delete(a.allowedTargets, oldestKey)
}

func (a *udpAssociation) targetKey(addr *net.UDPAddr) netip.AddrPort {
	key := udpAddrPort(addr)
	if !a.server.cfg.UDP.StrictEndpoint {
		// RFC 1928 does not require a response to use the request's destination
		// port. Protocols such as TFTP deliberately reply from a negotiated
		// server port, so the compatible default authorizes the contacted IP.
		key = netip.AddrPortFrom(key.Addr(), 0)
	}
	return key
}

func udpAddrPort(addr *net.UDPAddr) netip.AddrPort {
	ip, _ := netip.AddrFromSlice(addr.IP)
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if addr.Zone != "" && ip.Is6() {
		ip = ip.WithZone(addr.Zone)
	}
	return netip.AddrPortFrom(ip, uint16(addr.Port))
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}
}

func (a *udpAssociation) closeSockets() {
	if a.client != nil {
		_ = a.client.Close()
	}
	a.mu.Lock()
	if a.out4 != nil {
		_ = a.out4.Close()
	}
	if a.out6 != nil {
		_ = a.out6.Close()
	}
	a.mu.Unlock()
}

func (a *udpAssociation) close() {
	a.closeSockets()
	a.wg.Wait()
}
