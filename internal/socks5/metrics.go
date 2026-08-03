package socks5

import (
	"net"
	"sync/atomic"
	"time"
)

type metricCounters struct {
	activeConnections  atomic.Int64
	totalConnections   atomic.Uint64
	connectionErrors   atomic.Uint64
	connectCommands    atomic.Uint64
	bindCommands       atomic.Uint64
	udpAssociations    atomic.Uint64
	activeUDP          atomic.Int64
	tcpUploadBytes     atomic.Uint64
	tcpDownloadBytes   atomic.Uint64
	udpUploadPackets   atomic.Uint64
	udpDownloadPackets atomic.Uint64
	udpUploadBytes     atomic.Uint64
	udpDownloadBytes   atomic.Uint64
}

type MetricsSnapshot struct {
	ActiveConnections  int64  `json:"active_connections"`
	TotalConnections   uint64 `json:"total_connections"`
	ConnectionErrors   uint64 `json:"connection_errors"`
	ConnectCommands    uint64 `json:"connect_commands"`
	BindCommands       uint64 `json:"bind_commands"`
	UDPAssociations    uint64 `json:"udp_associations"`
	ActiveUDP          int64  `json:"active_udp"`
	TCPUploadBytes     uint64 `json:"tcp_upload_bytes"`
	TCPDownloadBytes   uint64 `json:"tcp_download_bytes"`
	UDPUploadPackets   uint64 `json:"udp_upload_packets"`
	UDPDownloadPackets uint64 `json:"udp_download_packets"`
	UDPUploadBytes     uint64 `json:"udp_upload_bytes"`
	UDPDownloadBytes   uint64 `json:"udp_download_bytes"`
}

func (s *Server) Metrics() MetricsSnapshot {
	m := &s.metrics
	return MetricsSnapshot{
		ActiveConnections: m.activeConnections.Load(), TotalConnections: m.totalConnections.Load(), ConnectionErrors: m.connectionErrors.Load(),
		ConnectCommands: m.connectCommands.Load(), BindCommands: m.bindCommands.Load(), UDPAssociations: m.udpAssociations.Load(), ActiveUDP: m.activeUDP.Load(),
		TCPUploadBytes: m.tcpUploadBytes.Load(), TCPDownloadBytes: m.tcpDownloadBytes.Load(), UDPUploadPackets: m.udpUploadPackets.Load(),
		UDPDownloadPackets: m.udpDownloadPackets.Load(), UDPUploadBytes: m.udpUploadBytes.Load(), UDPDownloadBytes: m.udpDownloadBytes.Load(),
	}
}

func DeviceDialer(iface string, timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: bindToDevice(iface)}
}
