package socks5

import (
	"context"
	"fmt"
	"net"
	"time"
)

func (s *Server) handleBind(client net.Conn, requested address) error {
	network := "tcp4"
	if ip := net.ParseIP(requested.Host); ip != nil && ip.To4() == nil {
		network = "tcp6"
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.BindTimeout.Value(2*time.Minute))
	defer cancel()
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
	advertiseIP, err := interfaceIP(s.cfg.Interface, network == "tcp4")
	if err != nil {
		_ = writeReply(client, repGeneralFailure, nil)
		return err
	}
	if err := writeReply(client, repSuccess, &net.TCPAddr{IP: advertiseIP, Port: bound.Port}); err != nil {
		return err
	}
	if tcpLn, ok := ln.(*net.TCPListener); ok {
		_ = tcpLn.SetDeadline(time.Now().Add(s.cfg.BindTimeout.Value(2 * time.Minute)))
	}
	peer, err := ln.Accept()
	if err != nil {
		_ = writeReply(client, replyForError(err), nil)
		return fmt.Errorf("BIND accept: %w", err)
	}
	defer peer.Close()
	if !bindPeerAllowed(peer.RemoteAddr().(*net.TCPAddr), requested) {
		_ = writeReply(client, repNotAllowed, peer.RemoteAddr())
		return fmt.Errorf("BIND peer %s did not match requested %s", peer.RemoteAddr(), requested.String())
	}
	if err := writeReply(client, repSuccess, peer.RemoteAddr()); err != nil {
		return err
	}
	s.log.Debug("BIND", "client", client.RemoteAddr(), "peer", peer.RemoteAddr())
	return s.relayTCP(client, peer, s.cfg.IdleTimeout.Value(5*time.Minute))
}

func bindPeerAllowed(peer *net.TCPAddr, requested address) bool {
	if requested.Port != 0 && peer.Port != int(requested.Port) {
		return false
	}
	ip := net.ParseIP(requested.Host)
	return ip == nil || ip.IsUnspecified() || ip.Equal(peer.IP)
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
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ipv4 && ip.To4() != nil {
			return ip.To4(), nil
		}
		if !ipv4 && ip.To4() == nil && ip.To16() != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("interface %s has no suitable IP address", name)
}
