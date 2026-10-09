import type { ServerConfig } from '../types';
import { lines, splitList } from './format';

export interface Pair {
  key: string;
  value: string;
}
export interface Credential {
  username: string;
  password: string;
  original?: string;
  unchanged?: boolean;
}
export type ServerFormValues = ServerConfig & {
  auth_users: Credential[];
  dns_mode: string;
  dns_servers: string;
  doh_urls: string;
  doh_bootstrap: string;
  doh_headers: Pair[];
  admission_cidrs: string;
  target_rules: string;
  udp_ports: string;
  udp_map: Pair[];
  udp_source_map: Pair[];
};
const pairs = (values?: Record<string, string>) =>
  Object.entries(values || {}).map(([key, value]) => ({ key, value }));
export function defaultServer(): ServerConfig {
  return {
    name: '',
    interface: '',
    listen: '0.0.0.0:1080',
    enabled: true,
    vohive_device_id: '',
    max_connections: 1024,
    connect_timeout: '10s',
    idle_timeout: '5m',
    bind_timeout: '2m',
    bind: { enabled: false, advertise: 'auto' },
    auth: { method: 'none', users: {}, password_unchanged: [] },
    http_proxy: { enabled: false, listen: '0.0.0.0:8080' },
    upstream: { enabled: false, address: '', auth_method: 'none', username: '', password: '' },
    access: {
      admission_cidrs: [],
      target_default: 'allow',
      target_rules: [],
      max_connections_per_ip: 64,
      max_udp_associations_per_ip: 4,
    },
    dns: { ipv4_only: false },
    udp: {
      enabled: true,
      max_associations: 64,
      strict_endpoint: false,
      bind_ip: '0.0.0.0',
      advertise: 'auto',
      idle_timeout: '2m',
      relay_ports: [],
      port_min: 10000,
      port_max: 65535,
    },
    heartbeat: { url: 'https://1.1.1.1/cdn-cgi/trace', interval: '30s', timeout: '12s' },
  };
}
export function serverToForm(source?: ServerConfig): ServerFormValues {
  const defaults = defaultServer();
  const config: ServerConfig = { ...defaults, ...source };
  for (const key of [
    'bind',
    'auth',
    'access',
    'http_proxy',
    'upstream',
    'dns',
    'udp',
    'heartbeat',
  ] as const)
    Object.assign(config, { [key]: { ...defaults[key], ...source?.[key] } });
  // Upstream credentials are write-only; an empty password preserves the stored secret.
  config.upstream = { ...config.upstream, password: '' };
  config.dns = {
    ...config.dns,
    doh: config.dns.doh ? { ...config.dns.doh, headers: { ...config.dns.doh.headers } } : undefined,
  };
  return {
    ...config,
    dns: {
      ...config.dns,
      doh: { bootstrap_ips: [], timeout: '10s', insecure_skip_verify: false, ...config.dns.doh },
    },
    auth_users: Object.entries(config.auth.users || {}).map(([username]) => ({
      username,
      password: '',
      original: username,
      unchanged: config.auth.password_unchanged?.includes(username) ?? false,
    })),
    dns_mode: config.dns.doh ? 'doh' : config.dns.servers?.length ? 'servers' : 'system',
    dns_servers: (config.dns.servers || []).join('\n'),
    doh_urls: (config.dns.doh?.urls?.length ? config.dns.doh.urls : [config.dns.doh?.url])
      .filter(Boolean)
      .join('\n'),
    doh_bootstrap: (config.dns.doh?.bootstrap_ips || []).join(', '),
    doh_headers: pairs(config.dns.doh?.headers),
    admission_cidrs: (config.access.admission_cidrs || []).join('\n'),
    target_rules: (config.access.target_rules || []).join('\n'),
    udp_ports: (config.udp.relay_ports?.length
      ? config.udp.relay_ports
      : config.udp.relay_port
        ? [config.udp.relay_port]
        : []
    ).join(', '),
    udp_map: pairs(config.udp.advertise_map),
    udp_source_map: pairs(config.udp.advertise_source_map),
  };
}
export function parsePorts(value: string) {
  const ports = splitList(value).map(Number);
  if (ports.length > 4096) throw new Error('固定端口池最多包含 4096 个端口');
  if (ports.some((port) => !Number.isInteger(port) || port < 1024 || port > 65535))
    throw new Error('UDP 固定端口必须是 1024–65535 的整数');
  if (new Set(ports).size !== ports.length) throw new Error('UDP 固定端口不能重复');
  return ports;
}
function pairMap(values: Pair[] = []) {
  const result: Record<string, string> = Object.create(null);
  for (const pair of values) {
    const key = pair.key?.trim(),
      value = pair.value?.trim();
    if (!key || !value) throw new Error('映射的两列都需要填写');
    if (Object.hasOwn(result, key)) throw new Error('映射键不能重复：' + key);
    result[key] = value;
  }
  return result;
}
export function formToServer(value: ServerFormValues): ServerConfig {
  const {
    auth_users,
    dns_mode,
    dns_servers,
    doh_urls,
    doh_bootstrap,
    doh_headers,
    admission_cidrs,
    target_rules,
    udp_ports,
    udp_map,
    udp_source_map,
    ...config
  } = value;
  const users: Record<string, string> = Object.create(null),
    unchanged: string[] = [];
  for (const row of auth_users || []) {
    const username = row.username?.trim();
    if (!username || Object.hasOwn(users, username)) throw new Error('代理用户名不能为空或重复');
    users[username] = row.password || '';
    if (!row.password && row.unchanged && username === row.original) unchanged.push(username);
    else if (!row.password && config.auth.method === 'username_password')
      throw new Error('新建或重命名的用户需要设置密码');
  }
  return {
    ...config,
    name: config.name.trim(),
    interface: config.interface.trim(),
    listen: config.listen.trim(),
    vohive_device_id: (config.vohive_device_id || '').trim(),
    auth: { method: config.auth.method, users, password_unchanged: unchanged },
    dns: {
      ipv4_only: !!config.dns.ipv4_only,
      ...(dns_mode === 'servers'
        ? { servers: splitList(dns_servers) }
        : dns_mode === 'doh'
          ? {
              doh: {
                urls: lines(doh_urls),
                bootstrap_ips: splitList(doh_bootstrap),
                timeout: config.dns.doh?.timeout || '10s',
                insecure_skip_verify: !!config.dns.doh?.insecure_skip_verify,
                headers: pairMap(doh_headers),
              },
            }
          : {}),
    },
    access: {
      ...config.access,
      admission_cidrs: splitList(admission_cidrs),
      target_rules: lines(target_rules),
    },
    udp: {
      ...config.udp,
      relay_port: undefined,
      relay_ports: parsePorts(udp_ports),
      advertise_map: pairMap(udp_map),
      advertise_source_map: pairMap(udp_source_map),
    },
    upstream: {
      ...config.upstream,
      address: (config.upstream.address || '').trim(),
      username: (config.upstream.username || '').trim(),
      password: config.upstream.password || '',
    },
  };
}
