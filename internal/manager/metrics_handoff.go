package manager

import (
	"wwan-proxy/internal/httpproxy"
	"wwan-proxy/internal/socks5"
)

// generationMetrics contains cumulative counters only. Gauges are read live
// from both the current and retiring generations when snapshots are produced.
type generationMetrics struct {
	socks socks5.MetricsSnapshot
	http  httpproxy.MetricsSnapshot
}

// beginMetricHandoffLocked installs the old generation's current totals as a
// carry before the instance map is swapped. Snapshots therefore remain
// monotonic across a hot reload. m.mu must be held by the caller.
func (m *Manager) beginMetricHandoffLocked(inst *instance) {
	inst.mu.RLock()
	launched := inst.launched
	inst.mu.RUnlock()
	if !launched {
		return
	}
	base := generationMetrics{socks: inst.server.Metrics()}
	if inst.httpProxy != nil {
		base.http = inst.httpProxy.Metrics()
	}
	carry := m.metricCarry[inst.cfg.ID]
	carry.socks = addSocksMetrics(carry.socks, base.socks)
	carry.http = addHTTPMetrics(carry.http, base.http)
	m.metricCarry[inst.cfg.ID] = carry
	if m.retiringMetrics[inst.cfg.ID] == nil {
		m.retiringMetrics[inst.cfg.ID] = make(map[*instance]generationMetrics)
	}
	m.retiringMetrics[inst.cfg.ID][inst] = base
}

// finishMetricHandoff folds traffic completed while the old generation was
// draining into the permanent carry and removes its live gauge contribution.
func (m *Manager) finishMetricHandoff(inst *instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	generations := m.retiringMetrics[inst.cfg.ID]
	base, ok := generations[inst]
	if !ok {
		return
	}
	final := generationMetrics{socks: inst.server.Metrics()}
	if inst.httpProxy != nil {
		final.http = inst.httpProxy.Metrics()
	}
	carry := m.metricCarry[inst.cfg.ID]
	carry.socks = addSocksMetrics(carry.socks, subtractSocksMetrics(final.socks, base.socks))
	carry.http = addHTTPMetrics(carry.http, subtractHTTPMetrics(final.http, base.http))
	m.metricCarry[inst.cfg.ID] = carry
	delete(generations, inst)
	if len(generations) == 0 {
		delete(m.retiringMetrics, inst.cfg.ID)
	}
}

func addSocksMetrics(a, b socks5.MetricsSnapshot) socks5.MetricsSnapshot {
	a.TotalConnections += b.TotalConnections
	a.ConnectionErrors += b.ConnectionErrors
	a.AdmissionDrops += b.AdmissionDrops
	a.ConnectionLimitDrops += b.ConnectionLimitDrops
	a.TargetDenied += b.TargetDenied
	a.ConnectCommands += b.ConnectCommands
	a.BindCommands += b.BindCommands
	a.UDPAssociations += b.UDPAssociations
	a.TCPUploadBytes += b.TCPUploadBytes
	a.TCPDownloadBytes += b.TCPDownloadBytes
	a.UDPUploadPackets += b.UDPUploadPackets
	a.UDPDownloadPackets += b.UDPDownloadPackets
	a.UDPUploadBytes += b.UDPUploadBytes
	a.UDPDownloadBytes += b.UDPDownloadBytes
	a.UDPClientSourceDrops += b.UDPClientSourceDrops
	a.UDPFragmentDrops += b.UDPFragmentDrops
	a.UDPInvalidDrops += b.UDPInvalidDrops
	a.UDPTruncatedDrops += b.UDPTruncatedDrops
	a.UDPQueueDrops += b.UDPQueueDrops
	a.UDPResolveDrops += b.UDPResolveDrops
	a.UDPResponseSourceDrops += b.UDPResponseSourceDrops
	a.UDPSendErrors += b.UDPSendErrors
	return a
}

func subtractSocksMetrics(a, b socks5.MetricsSnapshot) socks5.MetricsSnapshot {
	return socks5.MetricsSnapshot{
		TotalConnections:       subtractCounter(a.TotalConnections, b.TotalConnections),
		ConnectionErrors:       subtractCounter(a.ConnectionErrors, b.ConnectionErrors),
		AdmissionDrops:         subtractCounter(a.AdmissionDrops, b.AdmissionDrops),
		ConnectionLimitDrops:   subtractCounter(a.ConnectionLimitDrops, b.ConnectionLimitDrops),
		TargetDenied:           subtractCounter(a.TargetDenied, b.TargetDenied),
		ConnectCommands:        subtractCounter(a.ConnectCommands, b.ConnectCommands),
		BindCommands:           subtractCounter(a.BindCommands, b.BindCommands),
		UDPAssociations:        subtractCounter(a.UDPAssociations, b.UDPAssociations),
		TCPUploadBytes:         subtractCounter(a.TCPUploadBytes, b.TCPUploadBytes),
		TCPDownloadBytes:       subtractCounter(a.TCPDownloadBytes, b.TCPDownloadBytes),
		UDPUploadPackets:       subtractCounter(a.UDPUploadPackets, b.UDPUploadPackets),
		UDPDownloadPackets:     subtractCounter(a.UDPDownloadPackets, b.UDPDownloadPackets),
		UDPUploadBytes:         subtractCounter(a.UDPUploadBytes, b.UDPUploadBytes),
		UDPDownloadBytes:       subtractCounter(a.UDPDownloadBytes, b.UDPDownloadBytes),
		UDPClientSourceDrops:   subtractCounter(a.UDPClientSourceDrops, b.UDPClientSourceDrops),
		UDPFragmentDrops:       subtractCounter(a.UDPFragmentDrops, b.UDPFragmentDrops),
		UDPInvalidDrops:        subtractCounter(a.UDPInvalidDrops, b.UDPInvalidDrops),
		UDPTruncatedDrops:      subtractCounter(a.UDPTruncatedDrops, b.UDPTruncatedDrops),
		UDPQueueDrops:          subtractCounter(a.UDPQueueDrops, b.UDPQueueDrops),
		UDPResolveDrops:        subtractCounter(a.UDPResolveDrops, b.UDPResolveDrops),
		UDPResponseSourceDrops: subtractCounter(a.UDPResponseSourceDrops, b.UDPResponseSourceDrops),
		UDPSendErrors:          subtractCounter(a.UDPSendErrors, b.UDPSendErrors),
	}
}

func addHTTPMetrics(a, b httpproxy.MetricsSnapshot) httpproxy.MetricsSnapshot {
	a.TotalRequests += b.TotalRequests
	a.RequestErrors += b.RequestErrors
	a.HTTPRequests += b.HTTPRequests
	a.ConnectTunnels += b.ConnectTunnels
	a.UploadBytes += b.UploadBytes
	a.DownloadBytes += b.DownloadBytes
	a.AdmissionDrops += b.AdmissionDrops
	a.LimitDrops += b.LimitDrops
	a.TargetDenied += b.TargetDenied
	return a
}

func subtractHTTPMetrics(a, b httpproxy.MetricsSnapshot) httpproxy.MetricsSnapshot {
	return httpproxy.MetricsSnapshot{
		TotalRequests:  subtractCounter(a.TotalRequests, b.TotalRequests),
		RequestErrors:  subtractCounter(a.RequestErrors, b.RequestErrors),
		HTTPRequests:   subtractCounter(a.HTTPRequests, b.HTTPRequests),
		ConnectTunnels: subtractCounter(a.ConnectTunnels, b.ConnectTunnels),
		UploadBytes:    subtractCounter(a.UploadBytes, b.UploadBytes),
		DownloadBytes:  subtractCounter(a.DownloadBytes, b.DownloadBytes),
		AdmissionDrops: subtractCounter(a.AdmissionDrops, b.AdmissionDrops),
		LimitDrops:     subtractCounter(a.LimitDrops, b.LimitDrops),
		TargetDenied:   subtractCounter(a.TargetDenied, b.TargetDenied),
	}
}

func subtractCounter(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return a - b
}
