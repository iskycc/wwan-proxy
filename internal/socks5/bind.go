package socks5

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"wwan-proxy/internal/policy"
)

type bindPeerConstraint struct {
	port int
	ips  []net.IP
}

func (s *Server) handleBind(client net.Conn, requested address) error {
	return s.handleBindContext(context.Background(), client, requested)
}

// handleBindContext implements the two-reply SOCKS5 BIND exchange. The caller
// should pass the server instance context so shutdown cancels a pending BIND
// even before the control connection is closed.
func (s *Server) handleBindContext(parent context.Context, client net.Conn, requested address) error {
	timeout := s.cfg.BindTimeout.Value(2 * time.Minute)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	constraint, network, err := s.resolveBindPeer(ctx, requested)
	if err != nil {
		_ = writeReply(client, replyForError(err), nil)
		return fmt.Errorf("BIND resolve %s: %w", requested.String(), err)
	}

	lc := net.ListenConfig{Control: bindToDevice(s.cfg.Interface), KeepAlive: 30 * time.Second}
	bindAddr := "0.0.0.0:0"
	if network == "tcp6" {
		bindAddr = "[::]:0"
	}
	ln, err := lc.Listen(ctx, network, bindAddr)
	if err != nil {
		_ = writeReply(client, replyForError(err), nil)
		return fmt.Errorf("BIND listen: %w", err)
	}
	defer ln.Close()

	bound := ln.Addr().(*net.TCPAddr)
	advertiseIP, err := s.bindAdvertiseIP(client, network == "tcp4")
	if err != nil {
		_ = writeReply(client, repGeneralFailure, nil)
		return err
	}
	if err := writeReply(client, repSuccess, &net.TCPAddr{IP: advertiseIP, Port: bound.Port}); err != nil {
		return err
	}

	peer, err := acceptBindPeer(ctx, ln, client, constraint, time.Now().Add(timeout), func(peer *net.TCPAddr) bool {
		if s.access == nil {
			return true
		}
		host := requested.Host
		if requestedIP := net.ParseIP(host); requestedIP != nil && requestedIP.IsUnspecified() {
			host = peer.IP.String()
		}
		if s.access.AllowTarget(host, peer.IP, peer.Port) {
			return true
		}
		s.metrics.targetDenied.Add(1)
		return false
	})
	if err != nil {
		// The first success reply has already been sent. RFC 1928 requires a
		// second reply when the inbound operation fails, unless the control
		// connection itself has gone away.
		if !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			_ = writeReply(client, replyForError(err), nil)
		}
		return fmt.Errorf("BIND accept: %w", err)
	}
	defer peer.Close()
	if !s.trackOutbound(peer) {
		return net.ErrClosed
	}
	defer s.track(peer, false)
	if err := writeReply(client, repSuccess, peer.RemoteAddr()); err != nil {
		return err
	}
	s.log.Debug("BIND", "client", client.RemoteAddr(), "peer", peer.RemoteAddr())
	return s.relayTCP(client, peer, s.cfg.IdleTimeout.Value(5*time.Minute))
}

func (s *Server) resolveBindPeer(ctx context.Context, requested address) (bindPeerConstraint, string, error) {
	constraint := bindPeerConstraint{port: int(requested.Port)}
	ip := net.ParseIP(requested.Host)
	if ip != nil {
		// An unspecified address or port delegates the missing constraint to the
		// incoming peer. Its actual IP and port are authorized in the accept loop.
		allowed := true
		if s.access != nil && !ip.IsUnspecified() {
			if requested.Port == 0 {
				allowed = s.access.AllowTargetOnAnyPort(requested.Host, ip)
			} else {
				allowed = s.access.AllowTarget(requested.Host, ip, int(requested.Port))
			}
		}
		if !allowed {
			s.metrics.targetDenied.Add(1)
			return bindPeerConstraint{}, "", fmt.Errorf("%w: %s", policy.ErrTargetDenied, requested.String())
		}
		if !ip.IsUnspecified() {
			constraint.ips = []net.IP{ip}
		}
		if ip.To4() != nil {
			return constraint, "tcp4", nil
		}
		return constraint, "tcp6", nil
	}

	lookupNetwork := "ip"
	if s.cfg.DNS.IPv4Only {
		lookupNetwork = "ip4"
	}
	resolveCtx, cancel := context.WithTimeout(ctx, s.resolutionTimeout())
	defer cancel()
	ips, err := s.resolver.LookupIP(resolveCtx, lookupNetwork, requested.Host)
	if err != nil {
		return bindPeerConstraint{}, "", err
	}
	constraint.ips = uniqueIPs(ips)
	if s.access != nil && requested.Port != 0 {
		allowed := constraint.ips[:0]
		for _, ip := range constraint.ips {
			if s.access.AllowTarget(requested.Host, ip, int(requested.Port)) {
				allowed = append(allowed, ip)
			}
		}
		constraint.ips = allowed
	} else if s.access != nil {
		allowed := constraint.ips[:0]
		for _, ip := range constraint.ips {
			if s.access.AllowTargetOnAnyPort(requested.Host, ip) {
				allowed = append(allowed, ip)
			}
		}
		constraint.ips = allowed
	}
	if len(constraint.ips) == 0 {
		s.metrics.targetDenied.Add(1)
		return bindPeerConstraint{}, "", fmt.Errorf("%w: hostname resolved to no allowed addresses", policy.ErrTargetDenied)
	}
	// One BIND reply can advertise only one address. Select a family that is
	// usable both by the requested peer and by this server's advertised address.
	network, err := s.bindNetworkForResolvedIPs(constraint.ips)
	if err != nil {
		return bindPeerConstraint{}, "", err
	}
	return constraint, network, nil
}

func (s *Server) bindNetworkForResolvedIPs(ips []net.IP) (string, error) {
	ipv4Available, ipv6Available := true, true
	if configured := s.cfg.Bind.Advertise; configured != "" && configured != "auto" {
		advertiseIP := net.ParseIP(configured)
		if advertiseIP == nil {
			return "", fmt.Errorf("invalid configured BIND advertise address")
		}
		ipv4Available = advertiseIP.To4() != nil
		ipv6Available = !ipv4Available
	} else if s.cfg.Interface != "" {
		_, ipv4Err := interfaceIP(s.cfg.Interface, true)
		_, ipv6Err := interfaceIP(s.cfg.Interface, false)
		ipv4Available = ipv4Err == nil
		ipv6Available = ipv6Err == nil
	}
	return selectBindNetwork(ips, ipv4Available, ipv6Available)
}

func selectBindNetwork(ips []net.IP, ipv4Available, ipv6Available bool) (string, error) {
	hasIPv6 := false
	for _, resolved := range ips {
		if resolved.To4() != nil {
			if ipv4Available {
				return "tcp4", nil
			}
			continue
		}
		if resolved.To16() != nil {
			hasIPv6 = true
		}
	}
	if hasIPv6 && ipv6Available {
		return "tcp6", nil
	}
	return "", fmt.Errorf("BIND peer addresses and advertise address have no usable family in common")
}

func uniqueIPs(ips []net.IP) []net.IP {
	seen := make(map[string]struct{}, len(ips))
	result := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		canonical := ip.To16()
		if canonical == nil {
			continue
		}
		key := string(canonical)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, append(net.IP(nil), ip...))
	}
	return result
}

// acceptBindPeer watches the SOCKS control connection while accepting inbound
// peers. A connection from a source other than the requested peer is discarded
// and does not consume the single RFC 1928 BIND result.
func acceptBindPeer(ctx context.Context, ln net.Listener, client net.Conn, constraint bindPeerConstraint, deadline time.Time, allow func(*net.TCPAddr) bool) (net.Conn, error) {
	if tcpLn, ok := ln.(*net.TCPListener); ok {
		_ = tcpLn.SetDeadline(deadline)
	}

	controlResult := make(chan error, 1)
	go func() {
		var unexpected [1]byte
		_, err := client.Read(unexpected[:])
		if err == nil {
			err = fmt.Errorf("unexpected data on BIND control connection before second reply")
		}
		controlResult <- err
		_ = ln.Close()
	}()

	contextWatchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-contextWatchDone:
		}
	}()
	defer close(contextWatchDone)

	stopControlWatch := func() error {
		_ = client.SetReadDeadline(time.Now())
		err := <-controlResult
		_ = client.SetReadDeadline(time.Time{})
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return nil
		}
		return err
	}

	for {
		peer, err := ln.Accept()
		if err != nil {
			select {
			case controlErr := <-controlResult:
				_ = client.SetReadDeadline(time.Time{})
				return nil, controlErr
			default:
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = stopControlWatch()
				return nil, ctxErr
			}
			_ = stopControlWatch()
			return nil, err
		}
		tcpPeer, ok := peer.RemoteAddr().(*net.TCPAddr)
		if !ok || !bindPeerAllowed(tcpPeer, constraint) || allow != nil && !allow(tcpPeer) {
			_ = peer.Close()
			continue
		}
		if err := stopControlWatch(); err != nil {
			_ = peer.Close()
			return nil, err
		}
		return peer, nil
	}
}

func bindPeerAllowed(peer *net.TCPAddr, constraint bindPeerConstraint) bool {
	if constraint.port != 0 && peer.Port != constraint.port {
		return false
	}
	if len(constraint.ips) == 0 {
		return true
	}
	for _, allowed := range constraint.ips {
		if allowed.Equal(peer.IP) {
			return true
		}
	}
	return false
}

func (s *Server) bindAdvertiseIP(client net.Conn, ipv4 bool) (net.IP, error) {
	if s.cfg.Bind.Advertise != "" && s.cfg.Bind.Advertise != "auto" {
		ip := net.ParseIP(s.cfg.Bind.Advertise)
		if ip == nil || (ip.To4() != nil) != ipv4 {
			return nil, fmt.Errorf("configured BIND advertise address does not match requested address family")
		}
		return ip, nil
	}
	if s.cfg.Interface != "" {
		return interfaceIP(s.cfg.Interface, ipv4)
	}
	if local, ok := client.LocalAddr().(*net.TCPAddr); ok && local.IP != nil && !local.IP.IsUnspecified() {
		if ipv4 && local.IP.To4() != nil {
			return local.IP.To4(), nil
		}
		if !ipv4 && local.IP.To4() == nil && local.IP.To16() != nil {
			return local.IP, nil
		}
	}
	return nil, fmt.Errorf("BIND cannot determine an address to advertise")
}

func interfaceIP(name string, ipv4 bool) (net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var fallback net.IP
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ipv4 != (ip.To4() != nil) {
			continue
		}
		if ip.IsGlobalUnicast() {
			if ipv4 {
				return ip.To4(), nil
			}
			return ip, nil
		}
		// Loopback is a useful deterministic fallback for local deployments.
		// IPv6 link-local addresses are not usable in a SOCKS reply because RFC
		// 1928 has nowhere to carry the required interface zone.
		if fallback == nil && (ip.IsLoopback() || ipv4 && ip.IsLinkLocalUnicast()) {
			fallback = append(net.IP(nil), ip...)
		}
	}
	if fallback != nil {
		if ipv4 {
			return fallback.To4(), nil
		}
		return fallback, nil
	}
	return nil, fmt.Errorf("interface %s has no suitable IP address", name)
}
