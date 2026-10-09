import type { VohiveEvent } from '../types';

export const eventLabels: Record<string, string> = {
  degraded: '链路异常',
  recovery_started: '恢复开始',
  recovery_succeeded: '恢复成功',
  recovery_failed: '恢复失败',
  recovered: '已恢复',
};
export const eventColors: Record<string, string> = {
  degraded: 'red',
  recovery_started: 'orange',
  recovery_succeeded: 'blue',
  recovery_failed: 'red',
  recovered: 'green',
};
export function eventSummary(event: VohiveEvent) {
  const details = event.details || {};
  let devices = details.devices as
    Record<string, { healthy?: boolean; signal?: number; status?: string }> | undefined;
  if (!devices) {
    for (const candidate of [details.error, event.message]) {
      if (typeof candidate !== 'string') continue;
      const start = candidate.indexOf('{');
      if (start < 0) continue;
      try {
        const parsed = JSON.parse(candidate.slice(start));
        if (parsed.devices) {
          devices = parsed.devices;
          break;
        }
      } catch {}
    }
  }
  if (devices && typeof devices === 'object') {
    const normal: string[] = [],
      abnormal: string[] = [];
    Object.entries(devices)
      .sort(([a], [b]) => a.localeCompare(b, undefined, { numeric: true }))
      .forEach(([name, device]) => (device?.healthy ? normal : abnormal).push(name));
    return {
      message: '设备健康检查异常',
      chips: [
        ...(normal.length ? [{ text: normal.join('、') + ' 正常', color: 'green' }] : []),
        ...(abnormal.length ? [{ text: abnormal.join('、') + ' 异常', color: 'red' }] : []),
      ],
      devices,
    };
  }
  const raw = String(details.error || event.message || '');
  if (/context deadline exceeded|Client\.Timeout exceeded/i.test(raw))
    return { message: 'Vohive 健康检查超时', chips: [{ text: '请求超时', color: 'orange' }] };
  if (/429|too many|rate limit|登录受限/i.test(raw))
    return { message: 'Vohive 登录或请求受限', chips: [{ text: '请求限流', color: 'orange' }] };
  return { message: event.message, chips: [] };
}
