import type { Overview, ServerConfig } from '../types';

export function bytes(value = 0): string {
  if (value < 1024) return Math.round(value) + ' B';
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), 4);
  return (
    (value / 1024 ** index).toFixed(value / 1024 ** index >= 10 ? 1 : 2) +
    ' ' +
    ['B', 'KB', 'MB', 'GB', 'TB'][index]
  );
}
export const time = (value?: string | number) =>
  value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—';
export function duration(seconds = 0) {
  const days = Math.floor(seconds / 86400),
    hours = Math.floor((seconds % 86400) / 3600),
    minutes = Math.floor((seconds % 3600) / 60);
  return days
    ? days + ' 天 ' + hours + ' 小时'
    : hours
      ? hours + ' 小时 ' + minutes + ' 分'
      : minutes + ' 分钟';
}
export const splitList = (value = '') =>
  value
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);
export const lines = (value = '') =>
  value
    .split(/\r?\n/)
    .map((x) => x.trim())
    .filter(Boolean);
export function aggregate(overview?: Overview | null) {
  const result = {
    active: 0,
    total: 0,
    errors: 0,
    tcpUp: 0,
    tcpDown: 0,
    udpUp: 0,
    udpDown: 0,
    httpUp: 0,
    httpDown: 0,
    udpPackets: 0,
    udpErrors: 0,
  };
  for (const instance of overview?.instances || []) {
    const m = instance.metrics || {},
      h = instance.http_metrics || {};
    result.active += (m.active_connections || 0) + (m.active_udp || 0) + (h.active_requests || 0);
    result.total += (m.total_connections || 0) + (h.total_requests || 0);
    result.errors += (m.connection_errors || 0) + (h.request_errors || 0);
    result.tcpUp += m.tcp_upload_bytes || 0;
    result.tcpDown += m.tcp_download_bytes || 0;
    result.udpUp += m.udp_upload_bytes || 0;
    result.udpDown += m.udp_download_bytes || 0;
    result.httpUp += h.upload_bytes || 0;
    result.httpDown += h.download_bytes || 0;
    result.udpPackets += (m.udp_upload_packets || 0) + (m.udp_download_packets || 0);
    for (const key of [
      'udp_client_source_drops',
      'udp_fragment_drops',
      'udp_invalid_drops',
      'udp_truncated_drops',
      'udp_queue_drops',
      'udp_resolve_drops',
      'udp_response_source_drops',
      'udp_send_errors',
    ])
      result.udpErrors += m[key] || 0;
  }
  return result;
}
export function serverHealth(config: ServerConfig, overview: Overview) {
  const instance = overview.instances.find((i) => i.id === config.id),
    heartbeat = overview.heartbeats[config.id!];
  if (!config.enabled) return { status: 'default' as const, text: '已停用' };
  if (instance?.last_error || (heartbeat && !heartbeat.healthy))
    return { status: 'error' as const, text: '链路异常' };
  if (instance?.running && heartbeat?.healthy)
    return { status: 'success' as const, text: '运行正常' };
  return { status: 'processing' as const, text: '检测中' };
}
