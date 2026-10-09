export interface ServerConfig {
  id?: number;
  name: string;
  enabled: boolean;
  listen: string;
  interface: string;
  vohive_device_id: string;
  max_connections: number;
  connect_timeout: string;
  idle_timeout: string;
  bind_timeout: string;
  bind: { enabled: boolean; advertise: string };
  auth: { method: string; users?: Record<string, string>; password_unchanged?: string[] };
  access: {
    admission_cidrs?: string[];
    target_default: string;
    target_rules?: string[];
    max_connections_per_ip: number;
    max_udp_associations_per_ip: number;
  };
  http_proxy: { enabled: boolean; listen: string };
  upstream: {
    enabled: boolean;
    address: string;
    auth_method: string;
    username: string;
    password: string;
  };
  dns: {
    ipv4_only: boolean;
    servers?: string[];
    doh?: {
      url?: string;
      urls?: string[];
      bootstrap_ips: string[];
      timeout: string;
      headers?: Record<string, string>;
      insecure_skip_verify: boolean;
    };
  };
  udp: {
    enabled: boolean;
    max_associations: number;
    strict_endpoint: boolean;
    bind_ip: string;
    advertise: string;
    advertise_map?: Record<string, string>;
    advertise_source_map?: Record<string, string>;
    idle_timeout: string;
    relay_port?: number;
    relay_ports?: number[];
    port_min: number;
    port_max: number;
  };
  heartbeat: { url: string; interval: string; timeout: string };
}
export interface SystemSettings {
  web_listen: string;
  database_path: string;
  log_level: string;
  log_retention_days: number;
  session_lifetime: string;
  vohive: {
    enabled: boolean;
    base_url: string;
    username: string;
    password: string;
    consecutive_failures: number;
    cooldown: string;
  };
  current_web_listen?: string;
  current_database_path?: string;
  startup_database_path?: string;
  restart_required?: boolean;
}
export interface AuthStatus {
  initialized: boolean;
  authenticated: boolean;
  username?: string;
  expires_at?: string;
}
export interface NetworkInterface {
  index: number;
  name: string;
  mtu: number;
  flags: string;
  addresses: string[];
}
export type Metrics = Record<string, number>;
export interface Instance {
  id: number;
  name: string;
  enabled: boolean;
  running: boolean;
  listen: string;
  interface: string;
  started_at?: string;
  last_error?: string;
  http_listen?: string;
  http_running: boolean;
  metrics: Metrics;
  http_metrics: Metrics;
}
export interface Heartbeat {
  checked_at: string;
  healthy: boolean;
  latency_ms: number;
  status_code: number;
  public_ip: string;
  colo: string;
  error: string;
  trace: string;
}
export interface VohiveEvent {
  id: number;
  type: string;
  device_id: string;
  server_id?: number;
  message: string;
  details: Record<string, unknown>;
  created_at: string;
}
export interface Overview {
  service_instance_id: string;
  sampled_at: string;
  uptime_seconds: number;
  servers: ServerConfig[];
  instances: Instance[];
  heartbeats: Record<string, Heartbeat>;
  vohive_events: VohiveEvent[];
  process: {
    goroutines: number;
    heap_bytes: number;
    heap_live_bytes: number;
    sys_bytes: number;
    gc_cycles: number;
    websocket_clients: number;
  };
}
export interface LogEntry {
  id: number;
  timestamp: string;
  level: string;
  component: string;
  server_name: string;
  message: string;
  details: Record<string, unknown>;
}
export interface Session {
  id: string;
  created_at: string;
  expires_at: string;
  remote_addr: string;
  user_agent: string;
  current: boolean;
}
export interface UpdateInfo {
  current_version: string;
  platform: string;
  architecture: string;
  development_build: boolean;
  checked: boolean;
  update_available: boolean;
  install_supported: boolean;
  install_message?: string;
  latest?: { tag: string; version: string; url: string; published_at: string; asset_name: string };
  operation?: {
    state: string;
    started_at: string;
    finished_at?: string;
    version?: string;
    interface?: string;
    message?: string;
  };
}
export interface StatsPoint {
  bucket: string;
  upload_bytes: number;
  download_bytes: number;
  total_connections: number;
  connection_errors: number;
  total_requests: number;
  request_errors: number;
  active_connections: number;
  heartbeat_latency_ms: number;
  heartbeat_healthy: boolean;
  success_rate: number;
}
export interface StatsSummary {
  upload_bytes: number;
  download_bytes: number;
  avg_latency_ms: number;
  success_rate: number;
  peak_active_connections: number;
  total_buckets: number;
  healthy_buckets: number;
}
export interface TrafficPoint {
  at: number;
  upload: number;
  download: number;
}
export type PageKey =
  'overview' | 'configuration' | 'performance' | 'logs' | 'events' | 'statistics' | 'settings';
