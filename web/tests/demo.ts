import type { Page } from '@playwright/test';
import type { Backend } from './fixtures';
import type { LogEntry, VohiveEvent } from '../src/types';

// Representative data for visual review. Stress cases remain in layout.spec.ts.
export async function seedDemoData(page: Page, backend: Backend) {
  const now = Date.now();
  backend.auth = {
    initialized: true,
    authenticated: true,
    username: 'administrator',
    expires_at: new Date(now + 86400000).toISOString(),
  };
  const base = backend.overview.servers[0];
  backend.overview.servers = [
    '移动网络 01',
    '联通网络 02',
    '电信网络 03 · 华东生产出口',
    '备用 IPv6 出口',
  ].map((name, index) => ({
    ...structuredClone(base),
    id: index + 1,
    name,
    enabled: index < 3,
    interface: 'wwan' + index,
    listen: index === 3 ? '[2001:db8::10]:1083' : '0.0.0.0:' + (1080 + index),
    vohive_device_id: 'Y' + (index + 2),
    http_proxy: { enabled: index < 2, listen: '0.0.0.0:' + (8080 + index) },
    dns:
      index % 2
        ? { ipv4_only: false, servers: ['1.1.1.1:53', '8.8.8.8:53'] }
        : {
            ipv4_only: true,
            doh: {
              urls: ['https://cloudflare-dns.com/dns-query'],
              bootstrap_ips: ['1.1.1.1'],
              timeout: '10s',
              insecure_skip_verify: false,
            },
          },
  }));
  backend.overview.uptime_seconds = 3 * 86400 + 5 * 3600;
  backend.overview.process = {
    goroutines: 86,
    heap_bytes: 37 * 1024 ** 2,
    heap_live_bytes: 28 * 1024 ** 2,
    sys_bytes: 64 * 1024 ** 2,
    gc_cycles: 128,
    websocket_clients: 3,
  };
  backend.overview.instances = backend.overview.servers.map((server, index) => ({
    id: server.id!,
    name: server.name,
    enabled: server.enabled,
    running: server.enabled,
    http_running: server.enabled && server.http_proxy.enabled,
    listen: server.listen,
    interface: server.interface,
    started_at: new Date(now - 86400000).toISOString(),
    metrics: {
      active_connections: server.enabled ? 42 + index * 18 : 0,
      active_udp: server.enabled ? 8 + index * 3 : 0,
      total_connections: 18360 + index * 7600,
      tcp_upload_bytes: (12 + index * 4) * 1024 ** 3,
      tcp_download_bytes: (38 + index * 9) * 1024 ** 3,
      udp_upload_bytes: (320 + index * 120) * 1024 ** 2,
      udp_download_bytes: (850 + index * 180) * 1024 ** 2,
      udp_upload_packets: 238600 + index * 9200,
      udp_download_packets: 589200 + index * 13200,
      connection_errors: index === 2 ? 12 : 0,
      udp_queue_drops: index === 2 ? 3 : 0,
    },
    http_metrics: {
      active_requests: index < 2 ? 6 + index * 4 : 0,
      total_requests: index < 2 ? 12680 + index * 4500 : 0,
      upload_bytes: index < 2 ? (2 + index) * 1024 ** 3 : 0,
      download_bytes: index < 2 ? (8 + index * 2) * 1024 ** 3 : 0,
    },
  }));
  backend.overview.heartbeats = Object.fromEntries(
    backend.overview.servers
      .filter((server) => server.enabled)
      .map((server, index) => [
        server.id!,
        {
          checked_at: new Date(now).toISOString(),
          healthy: index < 2,
          latency_ms: 56 + index * 18,
          status_code: index < 2 ? 200 : 0,
          public_ip: '203.0.113.' + (21 + index),
          colo: 'HKG',
          trace: '',
          error: index < 2 ? '' : '连接超时：上游暂时无法访问，等待下一次心跳检查。',
        },
      ]),
  );
  backend.update = {
    ...backend.update,
    current_version: 'af56807147f2',
    checked: true,
    update_available: true,
    latest: {
      tag: 'build-ui-preview',
      version: 'ui-preview',
      url: 'https://github.com/iskycc/wwan-proxy/releases',
      published_at: new Date(now - 3600000).toISOString(),
      asset_name: 'wwan-proxy-linux-amd64.tar.gz',
    },
  };
  const reply = (value: unknown) => ({
    contentType: 'application/json',
    body: JSON.stringify(value),
  });
  const logs: LogEntry[] = Array.from({ length: 26 }, (_, index) => ({
    id: index + 1,
    timestamp: new Date(now - index * 42000).toISOString(),
    level: ['INFO', 'WARN', 'ERROR', 'INFO'][index % 4],
    component: ['heartbeat', 'manager', 'socks5', 'http'][index % 4],
    server_name: backend.overview.servers[index % 3].name,
    message: [
      '心跳检查成功，出口链路连接正常。',
      '出口响应耗时较长，已安排下一次健康检查。',
      '连接上游超时：dial tcp 198.51.100.24:1080: i/o timeout。',
      '代理连接完成，上传 128 KB，下载 2.40 MB。',
    ][index % 4],
    details: {
      interface: 'wwan' + (index % 3),
      latency_ms: 56 + index,
      remote_addr: '198.51.100.24:1080',
    },
  }));
  const events: VohiveEvent[] = [
    {
      id: 1,
      type: 'degraded',
      device_id: 'Y4',
      message: '设备健康检查异常',
      details: {
        devices: { Y2: { healthy: true }, Y3: { healthy: true }, Y4: { healthy: false } },
      },
    },
    {
      id: 2,
      type: 'recovery_started',
      device_id: 'Y4',
      message: '连续两次心跳失败，开始恢复设备移动数据网络。',
      details: { failures: 2, interface: 'wwan2' },
    },
    {
      id: 3,
      type: 'recovery_succeeded',
      device_id: 'Y3',
      message: '设备移动数据网络重启成功，等待出口心跳确认。',
      details: { action: 'restart_mobile_data' },
    },
    {
      id: 4,
      type: 'recovered',
      device_id: 'Y3',
      message: '出口心跳恢复正常，链路已重新连接。',
      details: { latency_ms: 74 },
    },
    {
      id: 5,
      type: 'recovery_failed',
      device_id: 'Y4',
      message: 'Vohive 恢复请求超时，将在冷却期结束后重试。',
      details: { error: 'context deadline exceeded', cooldown: '5m' },
    },
  ].map((event, index) => ({ ...event, created_at: new Date(now - index * 180000).toISOString() }));
  await page.route('**/api/logs?**', (route) => {
    const query = new URL(route.request().url()).searchParams;
    return route.fulfill(
      reply(
        logs.filter(
          (log) =>
            (!query.get('level') || log.level === query.get('level')) &&
            (!query.get('q') || (log.message + log.server_name).includes(query.get('q')!)),
        ),
      ),
    );
  });
  await page.route('**/api/vohive/events?**', (route) => {
    const query = new URL(route.request().url()).searchParams;
    return route.fulfill(
      reply(
        events.filter(
          (event) =>
            (!query.get('type') || event.type === query.get('type')) &&
            (!query.get('device') || event.device_id === query.get('device')),
        ),
      ),
    );
  });
  await page.route('**/api/sessions', (route) =>
    route.fulfill(
      reply([
        {
          id: 'a'.repeat(64),
          current: true,
          user_agent:
            'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/157.0.0.0 Safari/537.36',
          remote_addr: '192.168.10.25:52140',
          created_at: new Date(now - 5400000).toISOString(),
          expires_at: backend.auth.expires_at,
        },
        {
          id: 'b'.repeat(64),
          current: false,
          user_agent:
            'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1',
          remote_addr: '[2001:db8::42]:53201',
          created_at: new Date(now - 7200000).toISOString(),
          expires_at: backend.auth.expires_at,
        },
      ]),
    ),
  );
  await page.route('**/api/stats?**', (route) => {
    const query = new URL(route.request().url()).searchParams,
      from = Date.parse(query.get('from')!),
      to = Date.parse(query.get('to')!);
    return route.fulfill(
      reply(
        Array.from({ length: 72 }, (_, index) => ({
          bucket: new Date(from + (index * (to - from)) / 71).toISOString(),
          upload_bytes: 1024 ** 2 * (16 + 5 * Math.sin(index / 4)),
          download_bytes: 1024 ** 2 * (35 + 9 * Math.cos(index / 6)),
          heartbeat_latency_ms: 65 + 20 * Math.sin(index / 8),
          heartbeat_healthy: index % 13 !== 0,
          success_rate: 0.96 + (index % 5) / 125,
        })),
      ),
    );
  });
  await page.route('**/api/stats/summary?**', (route) =>
    route.fulfill(
      reply({
        upload_bytes: 18.4 * 1024 ** 3,
        download_bytes: 68.2 * 1024 ** 3,
        avg_latency_ms: 67.8,
        success_rate: 0.9926,
        peak_active_connections: 218,
        total_buckets: 72,
        healthy_buckets: 68,
      }),
    ),
  );
  const at = Date.parse(backend.overview.sampled_at);
  await page.addInitScript(
    (history) =>
      localStorage.setItem('wwan-control.traffic.administrator', JSON.stringify(history)),
    {
      service: backend.overview.service_instance_id,
      at: at - 1000,
      baselines: Object.fromEntries(
        backend.overview.instances.map((instance) => [
          instance.id,
          {
            generation: instance.started_at,
            upload:
              instance.metrics.tcp_upload_bytes +
              instance.metrics.udp_upload_bytes +
              (instance.http_metrics.upload_bytes || 0) -
              1024 ** 2,
            download:
              instance.metrics.tcp_download_bytes +
              instance.metrics.udp_download_bytes +
              (instance.http_metrics.download_bytes || 0) -
              2 * 1024 ** 2,
          },
        ]),
      ),
      points: Array.from({ length: 60 }, (_, index) => ({
        at: at - (60 - index) * 4000,
        upload: 1024 ** 2 * (3 + Math.sin(index / 3)),
        download: 1024 ** 2 * (8 + Math.cos(index / 4)),
      })),
    },
  );
}
