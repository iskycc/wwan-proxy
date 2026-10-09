import { useCallback, useEffect, useRef, useState } from 'react';
import type { Overview, TrafficPoint } from '../types';

type Baseline = { generation: string; upload: number; download: number };
type History = {
  service: string;
  at: number;
  baselines: Record<string, Baseline>;
  points: TrafficPoint[];
};
const WINDOW = 5 * 60 * 1000;
const emptyHistory = (): History => ({ service: '', at: 0, baselines: {}, points: [] });
function loadHistory(username: string): History {
  try {
    const value = JSON.parse(localStorage.getItem('wwan-control.traffic.' + username) || 'null');
    if (
      value &&
      typeof value.service === 'string' &&
      Number.isFinite(value.at) &&
      value.baselines &&
      Array.isArray(value.points)
    ) {
      value.points = value.points
        .filter(
          (point: TrafficPoint) =>
            [point.at, point.upload, point.download].every(Number.isFinite) &&
            point.upload >= 0 &&
            point.download >= 0,
        )
        .slice(-300);
      for (const baseline of Object.values(value.baselines) as Baseline[])
        if (
          !baseline ||
          typeof baseline.generation !== 'string' ||
          !Number.isFinite(baseline.upload) ||
          !Number.isFinite(baseline.download)
        )
          return emptyHistory();
      return value;
    }
  } catch {
    /* Storage may be unavailable in a private browser. */
  }
  return emptyHistory();
}
function sample(history: History, overview: Overview): History {
  const at = Date.parse(overview.sampled_at);
  if (!Number.isFinite(at)) return history;
  if (history.service && history.service !== overview.service_instance_id) history = emptyHistory();
  history.service = overview.service_instance_id;
  history.points = history.points.filter((point) => point.at >= at - WINDOW && point.at <= at);
  const baselines: Record<string, Baseline> = {};
  let upload = 0,
    download = 0,
    matched = false;
  for (const instance of overview.instances) {
    const m = instance.metrics || {},
      h = instance.http_metrics || {};
    const current = {
      generation: instance.started_at || '',
      upload: (m.tcp_upload_bytes || 0) + (m.udp_upload_bytes || 0) + (h.upload_bytes || 0),
      download: (m.tcp_download_bytes || 0) + (m.udp_download_bytes || 0) + (h.download_bytes || 0),
    };
    baselines[instance.id] = current;
    const before = history.baselines[instance.id];
    if (
      before &&
      before.generation === current.generation &&
      current.upload >= before.upload &&
      current.download >= before.download
    ) {
      upload += current.upload - before.upload;
      download += current.download - before.download;
      matched = true;
    }
  }
  const elapsed = at - history.at;
  if (elapsed > 0 && elapsed < 750) return history;
  if (matched && elapsed >= 750 && elapsed <= WINDOW)
    history.points.push({
      at,
      upload: upload / (elapsed / 1000),
      download: download / (elapsed / 1000),
    });
  history.points = history.points.slice(-300);
  history.at = at;
  history.baselines = baselines;
  return history;
}

export function useOverview(enabled: boolean, username: string, onExpired: () => void) {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [history, setHistory] = useState<TrafficPoint[]>([]);
  const [connection, setConnection] = useState<'connecting' | 'connected' | 'disconnected'>(
    'connecting',
  );
  const socketRef = useRef<WebSocket | null>(null);
  const reconnectRef = useRef<() => void>(() => {});
  const pending = useRef<{
    promise: Promise<void>;
    resolve: () => void;
    reject: (error: Error) => void;
    timer: ReturnType<typeof setTimeout>;
  } | null>(null);
  useEffect(() => {
    if (!enabled) {
      setOverview(null);
      setHistory([]);
      return;
    }
    let stopped = false,
      attempts = 0,
      retry: ReturnType<typeof setTimeout> | undefined,
      socket: WebSocket | null = null;
    let samples = loadHistory(username),
      lastSave = 0;
    setHistory(samples.points);
    const save = () => {
      try {
        localStorage.setItem('wwan-control.traffic.' + username, JSON.stringify(samples));
      } catch {}
    };
    const settle = (error?: Error) => {
      if (!pending.current) return;
      clearTimeout(pending.current.timer);
      error ? pending.current.reject(error) : pending.current.resolve();
      pending.current = null;
    };
    const connect = () => {
      if (stopped) return;
      clearTimeout(retry);
      if (socket) {
        socket.onclose = null;
        socket.close();
      }
      setConnection('connecting');
      socket = new WebSocket(
        (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/ws',
      );
      socketRef.current = socket;
      const current = socket;
      current.onopen = () => {
        if (!stopped && socket === current) {
          attempts = 0;
          setConnection('connected');
        }
      };
      current.onmessage = (event) => {
        if (stopped || socket !== current) return;
        try {
          const value = JSON.parse(event.data) as Overview;
          if (!Array.isArray(value.servers) || !Array.isArray(value.instances) || !value.process)
            throw new Error('实时状态格式无效');
          setOverview(value);
          samples = sample(samples, value);
          setHistory([...samples.points]);
          if (Date.now() - lastSave >= 3000) {
            save();
            lastSave = Date.now();
          }
          settle();
        } catch {
          settle(new Error('无法读取实时状态，请刷新重试'));
        }
      };
      current.onerror = () => {};
      current.onclose = (event) => {
        if (stopped || socket !== current) return;
        socketRef.current = null;
        setConnection('disconnected');
        if (event.code === 1008) {
          onExpired();
          return;
        }
        retry = setTimeout(
          connect,
          [1000, 2000, 4000, 8000, 15000, 30000][Math.min(attempts++, 5)],
        );
      };
    };
    reconnectRef.current = connect;
    const online = () => connect();
    const visible = () => {
      if (
        document.visibilityState === 'visible' &&
        (!socketRef.current || socketRef.current.readyState === WebSocket.CLOSED)
      )
        connect();
    };
    window.addEventListener('online', online);
    window.addEventListener('pagehide', save);
    document.addEventListener('visibilitychange', visible);
    connect();
    return () => {
      stopped = true;
      clearTimeout(retry);
      save();
      socketRef.current = null;
      if (socket) {
        socket.onclose = null;
        socket.close();
      }
      settle(new Error('实时连接已关闭'));
      window.removeEventListener('online', online);
      window.removeEventListener('pagehide', save);
      document.removeEventListener('visibilitychange', visible);
    };
  }, [enabled, username, onExpired]);
  const refresh = useCallback(() => {
    if (pending.current) return pending.current.promise;
    let resolve!: () => void, reject!: (error: Error) => void;
    const promise = new Promise<void>((yes, no) => {
      resolve = yes;
      reject = no;
    });
    const timer = setTimeout(() => {
      pending.current = null;
      reject(new Error('实时状态同步超时'));
    }, 6000);
    pending.current = { promise, resolve, reject, timer };
    if (socketRef.current?.readyState === WebSocket.OPEN) socketRef.current.send('refresh');
    else if (socketRef.current?.readyState !== WebSocket.CONNECTING) reconnectRef.current();
    return promise;
  }, []);
  return { overview, history, connection, refresh };
}
