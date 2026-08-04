package socks5

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type udpAssociation struct {
	server   *Server
	client   *net.UDPConn
	peerIP   net.IP
	peerPort int

	mu       sync.RWMutex
	clientAt *net.UDPAddr
	out4     *net.UDPConn
	out6     *net.UDPConn
	wg       sync.WaitGroup
	lastUnix atomic.Int64
}

func (s *Server) handleUDP(control net.Conn, requested address) error {
	localTCP, ok := control.LocalAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("unexpected control address %T", control.LocalAddr())
	}
	remoteTCP := control.RemoteAddr().(*net.TCPAddr)
	requestedIP := net.ParseIP(requested.Host)
	if requestedIP != nil && !requestedIP.IsUnspecified() && !requestedIP.Equal(remoteTCP.IP) {
		_ = writeReply(control, repNotAllowed, nil)
		return fmt.Errorf("UDP requested client IP %s does not match TCP peer %s", requestedIP, remoteTCP.IP)
	}
	bindIP := net.ParseIP(s.cfg.UDP.BindIP)
	network := "udp4"
	if bindIP.To4() == nil {
		network = "udp6"
	}
	clientConn, err := listenRandomUDP(network, bindIP, s.cfg.UDP.PortMin, s.cfg.UDP.PortMax)
	if err != nil {
		_ = writeReply(control, replyForError(err), nil)
		return fmt.Errorf("UDP relay listen: %w", err)
	}
	defer clientConn.Close()
	advertiseIP, err := s.udpAdvertiseIP(localTCP.IP)
	if err != nil {
		_ = writeReply(control, repGeneralFailure, nil)
		return err
	}
	relayPort := clientConn.LocalAddr().(*net.UDPAddr).Port
	if err := writeReply(control, repSuccess, &net.UDPAddr{IP: advertiseIP, Port: relayPort}); err != nil {
		return err
	}

	assoc := &udpAssociation{server: s, client: clientConn, peerIP: remoteTCP.IP}
	assoc.peerPort = int(requested.Port)
	assoc.lastUnix.Store(time.Now().UnixNano())
	s.metrics.activeUDP.Add(1)
	defer s.metrics.activeUDP.Add(-1)
	defer assoc.close()
	s.log.Debug("UDP ASSOCIATE", "client", control.RemoteAddr(), "relay", net.JoinHostPort(advertiseIP.String(), fmt.Sprint(relayPort)))

	controlDone := make(chan struct{})
	go func() {
		var one [1]byte
		_, _ = control.Read(one[:])
		close(controlDone)
	}()
	return assoc.loop(controlDone, s.cfg.UDP.IdleTimeout.Value(2*time.Minute))
}

func (s *Server) udpAdvertiseIP(controlLocal net.IP) (net.IP, error) {
	key := controlLocal.String()
	if mapped := s.cfg.UDP.AdvertiseMap[key]; mapped != "" {
		return net.ParseIP(mapped), nil
	}
	if s.cfg.UDP.Advertise != "auto" {
		return net.ParseIP(s.cfg.UDP.Advertise), nil
	}
	if controlLocal == nil || controlLocal.IsUnspecified() {
		return nil, fmt.Errorf("cannot automatically determine UDP relay IP from control connection")
	}
	return controlLocal, nil
}

func (a *udpAssociation) loop(controlDone <-chan struct{}, idle time.Duration) error {
	buf := make([]byte, 65535)
	for {
		_ = a.client.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := a.client.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-controlDone:
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				last := time.Unix(0, a.lastUnix.Load())
				if time.Since(last) >= idle {
					return nil
				}
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !a.acceptClient(from) {
			continue
		}
		dst, payload, err := parseUDPDatagram(buf[:n])
		if err != nil {
			continue
		}
		target, err := a.resolve(dst)
		if err != nil {
			continue
		}
		out, err := a.outbound(target.IP.To4() != nil)
		if err != nil {
			return err
		}
		if _, err := out.WriteToUDP(payload, target); err == nil {
			a.server.metrics.udpUploadPackets.Add(1)
			a.server.metrics.udpUploadBytes.Add(uint64(len(payload)))
			a.lastUnix.Store(time.Now().UnixNano())
		}
	}
}

func (a *udpAssociation) acceptClient(from *net.UDPAddr) bool {
	if !from.IP.Equal(a.peerIP) {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.peerPort != 0 && from.Port != a.peerPort {
		return false
	}
	if a.clientAt == nil {
		a.clientAt = &net.UDPAddr{IP: append(net.IP(nil), from.IP...), Port: from.Port, Zone: from.Zone}
		if a.peerPort == 0 {
			a.peerPort = from.Port
		}
	}
	return from.Port == a.clientAt.Port
}

func (a *udpAssociation) resolve(dst address) (*net.UDPAddr, error) {
	if ip := net.ParseIP(dst.Host); ip != nil {
		return &net.UDPAddr{IP: ip, Port: int(dst.Port)}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.server.resolutionTimeout())
	defer cancel()
	ips, err := a.server.resolver.LookupIPAddr(ctx, dst.Host)
	if err != nil || len(ips) == 0 {
		if err == nil {
			err = fmt.Errorf("no address")
		}
		return nil, err
	}
	return &net.UDPAddr{IP: ips[0].IP, Zone: ips[0].Zone, Port: int(dst.Port)}, nil
}

func (a *udpAssociation) outbound(ipv4 bool) (*net.UDPConn, error) {
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
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("unexpected packet connection %T", pc)
	}
	*slot = conn
	a.wg.Add(1)
	go a.readResponses(conn)
	return conn, nil
}

func (a *udpAssociation) readResponses(conn *net.UDPConn) {
	defer a.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		a.mu.RLock()
		clientAt := a.clientAt
		a.mu.RUnlock()
		if clientAt == nil {
			continue
		}
		packet := makeUDPDatagram(from, buf[:n])
		if _, err := a.client.WriteToUDP(packet, clientAt); err == nil {
			a.server.metrics.udpDownloadPackets.Add(1)
			a.server.metrics.udpDownloadBytes.Add(uint64(n))
			a.lastUnix.Store(time.Now().UnixNano())
		}
	}
}

func (a *udpAssociation) close() {
	a.mu.Lock()
	if a.out4 != nil {
		_ = a.out4.Close()
	}
	if a.out6 != nil {
		_ = a.out6.Close()
	}
	a.mu.Unlock()
	a.wg.Wait()
}
