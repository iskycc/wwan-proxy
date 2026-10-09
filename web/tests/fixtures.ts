import { test as base, expect, type WebSocketRoute } from '@playwright/test';
import { defaultServer } from '../src/lib/serverForm';
import type {
  AuthStatus,
  LogEntry,
  Overview,
  ServerConfig,
  SystemSettings,
  UpdateInfo,
  VohiveEvent,
} from '../src/types';

export interface Backend {
  auth: AuthStatus;
  settings: SystemSettings;
  overview: Overview;
  update: UpdateInfo;
  requests: { path: string; method: string; body: any }[];
  sockets: WebSocketRoute[];
  settingsError: boolean;
  settingsDelay: number;
  push: () => void;
  errors: string[];
}
export const test = base.extend<{ backend: Backend }>({
  backend: async ({ page }, use) => {
    const server: ServerConfig = {
      ...defaultServer(),
      id: 1,
      name: '移动网络 01',
      enabled: false,
      interface: 'lo',
      vohive_device_id: 'Y2',
      auth: { method: 'username_password', users: { alice: '' }, password_unchanged: ['alice'] },
      udp: { ...defaultServer().udp, relay_ports: [12000, 12007] },
    };
    const backend: Backend = {
      auth: {
        initialized: true,
        authenticated: true,
        username: 'administrator',
        expires_at: new Date(Date.now() + 86400000).toISOString(),
      },
      settings: {
        web_listen: '127.0.0.1:9090',
        database_path: '/tmp/wwan-control.db',
        current_web_listen: '127.0.0.1:9090',
        current_database_path: '/tmp/wwan-control.db',
        log_level: 'WARN',
        log_retention_days: 30,
        session_lifetime: '24h',
        vohive: {
          enabled: true,
          base_url: 'http://127.0.0.1:9999',
          username: 'admin',
          password: 'vohive-password',
          consecutive_failures: 2,
          cooldown: '5m',
        },
      },
      overview: {
        service_instance_id: 'test-service',
        sampled_at: new Date().toISOString(),
        uptime_seconds: 3600,
        servers: [server],
        instances: [],
        heartbeats: {},
        vohive_events: [],
        process: {
          goroutines: 20,
          heap_bytes: 2097152,
          heap_live_bytes: 1048576,
          sys_bytes: 4194304,
          gc_cycles: 3,
          websocket_clients: 1,
        },
      },
      update: {
        current_version: 'test-version',
        platform: 'alpine',
        architecture: 'amd64',
        development_build: false,
        checked: false,
        update_available: false,
        install_supported: true,
      },
      requests: [],
      sockets: [],
      settingsError: false,
      settingsDelay: 0,
      errors: [],
      push: () => {},
    };
    backend.push = () => {
      backend.overview.sampled_at = new Date().toISOString();
      for (const socket of backend.sockets) {
        try {
          socket.send(JSON.stringify(backend.overview));
        } catch {}
      }
    };
    page.on('pageerror', (error) => backend.errors.push(error.message));
    const logs: LogEntry[] = [
      {
        id: 1,
        timestamp: new Date().toISOString(),
        level: 'ERROR',
        component: 'manager',
        server_name: server.name,
        message: '测试心跳错误',
        details: { stage: 'tcp', error: 'connection refused' },
      },
    ];
    const events: VohiveEvent[] = [
      {
        id: 1,
        type: 'degraded',
        device_id: 'Y2',
        message: 'health degraded',
        details: { devices: { Y2: { healthy: false }, Y3: { healthy: true } } },
        created_at: new Date().toISOString(),
      },
    ];
    await page.route('**/api/**', async (route) => {
      const request = route.request(),
        url = new URL(request.url());
      const method = request.method(),
        body = request.postDataJSON();
      backend.requests.push({ path: url.pathname + url.search, method, body });
      const reply = (value: unknown, status = 200) =>
        route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });
      if (url.pathname === '/api/auth/status') return reply(backend.auth);
      if (['/api/auth/login', '/api/auth/initialize'].includes(url.pathname)) {
        backend.auth = {
          ...backend.auth,
          authenticated: true,
          initialized: true,
          username: body.username,
        };
        return reply(backend.auth);
      }
      if (url.pathname === '/api/auth/logout') {
        backend.auth.authenticated = false;
        return route.fulfill({ status: 204 });
      }
      if (url.pathname === '/api/settings') {
        if (backend.settingsDelay)
          await new Promise((resolve) => setTimeout(resolve, backend.settingsDelay));
        if (backend.settingsError) return reply({ error: 'settings unavailable' }, 500);
        if (method === 'PUT')
          backend.settings = {
            ...body,
            current_database_path: body.database_path,
            current_web_listen: body.web_listen,
            restart_required: true,
          };
        return reply(backend.settings);
      }
      if (url.pathname === '/api/interfaces')
        return reply([
          { index: 1, name: 'lo', mtu: 65536, flags: 'up', addresses: ['127.0.0.1/8'] },
        ]);
      if (url.pathname === '/api/admin') {
        backend.auth.username = body.username;
        return reply({ username: body.username });
      }
      if (url.pathname === '/api/sessions')
        return reply([
          {
            id: 'a'.repeat(64),
            current: true,
            remote_addr: '127.0.0.1',
            user_agent: 'Test browser',
            created_at: new Date().toISOString(),
            expires_at: backend.auth.expires_at,
          },
        ]);
      if (url.pathname.startsWith('/api/sessions/')) return route.fulfill({ status: 204 });
      if (url.pathname === '/api/update') {
        if (method === 'POST')
          backend.update.operation = {
            state: 'queued',
            started_at: new Date().toISOString(),
            message: '排队中',
            interface: body.interface,
          };
        if (url.searchParams.has('refresh'))
          backend.update = {
            ...backend.update,
            checked: true,
            update_available: true,
            latest: {
              tag: 'build-new',
              version: 'new-version',
              url: 'https://github.com/iskycc/wwan-proxy/releases',
              published_at: new Date().toISOString(),
              asset_name: 'test.tar.gz',
            },
          };
        return reply(backend.update);
      }
      if (url.pathname.startsWith('/api/servers')) {
        if (url.pathname.endsWith('/toggle'))
          backend.overview.servers[0].enabled = !backend.overview.servers[0].enabled;
        else if (method === 'PUT') backend.overview.servers[0] = { ...body, id: 1 };
        else if (method === 'POST') backend.overview.servers.push({ ...body, id: 2 });
        else if (method === 'DELETE') backend.overview.servers = [];
        backend.push();
        return reply(body || backend.overview.servers);
      }
      if (url.pathname === '/api/logs') return reply(logs);
      if (url.pathname === '/api/vohive/events') return reply(events);
      if (url.pathname === '/api/stats/summary')
        return reply({
          upload_bytes: 1024,
          download_bytes: 2048,
          avg_latency_ms: 25,
          success_rate: 0.98,
          peak_active_connections: 20,
          total_buckets: 2,
          healthy_buckets: 2,
        });
      if (url.pathname === '/api/stats')
        return reply(
          [0, 1].map((index) => ({
            bucket: new Date(Date.now() - (1 - index) * 60000).toISOString(),
            upload_bytes: 1024 + index * 512,
            download_bytes: 2048,
            heartbeat_latency_ms: 25,
            heartbeat_healthy: true,
            success_rate: 0.98,
          })),
        );
      return reply({ error: 'Unhandled API ' + url.pathname }, 404);
    });
    await page.routeWebSocket('**/api/ws', (socket) => {
      backend.sockets.push(socket);
      socket.send(JSON.stringify(backend.overview));
      socket.onMessage((message) => {
        if (message === 'refresh') backend.push();
      });
    });
    await use(backend);
    expect(backend.errors).toEqual([]);
  },
});
export { expect };
