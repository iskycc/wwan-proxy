import { Line } from '@ant-design/plots';
import { Empty, Grid, theme } from 'antd';
import { bytes } from '../lib/format';

export default function TrafficChart({
  points,
  fields,
  unit = 'bytes',
  height = 280,
}: {
  points: Array<Record<string, string | number>>;
  fields: { key: string; label: string }[];
  unit?: 'bytes' | 'rate' | 'latency' | 'percent';
  height?: number;
}) {
  const { token } = theme.useToken();
  const compact = !Grid.useBreakpoint().md;
  const data = points
    .flatMap((point) =>
      fields.map((field) => ({
        at: Number(point.at),
        value: Number(point[field.key]) || 0,
        series: field.label,
      })),
    )
    .filter((point) => Number.isFinite(point.at));
  const times = data.map((point) => point.at);
  const span = times.length ? Math.max(...times) - Math.min(...times) : 0;
  const format = (value: number) =>
    unit === 'percent'
      ? (value * 100).toFixed(1) + '%'
      : unit === 'latency'
        ? Math.round(value) + ' ms'
        : bytes(value) + (unit === 'rate' ? '/s' : '');
  if (!data.length)
    return (
      <div className="chart-empty" style={{ height }}>
        <Empty description="所选时段暂无数据" />
      </div>
    );
  return (
    <div
      className="chart-wrap"
      role="img"
      aria-label={fields.map((field) => field.label).join('与') + '趋势图'}
    >
      <Line
        height={height}
        data={data}
        xField="at"
        yField="value"
        colorField="series"
        scale={{
          x: { type: 'time' },
          y: { domainMin: 0 },
          color: { range: [token.colorPrimary, '#13c2c2', '#faad14'] },
        }}
        axis={{
          x: {
            tickCount: compact ? 3 : 5,
            labelAutoHide: true,
            labelAutoRotate: false,
            labelFormatter: (value: number) =>
              span >= 2 * 86400000
                ? new Date(Number(value)).toLocaleDateString('zh-CN', {
                    month: 'numeric',
                    day: 'numeric',
                  })
                : new Date(Number(value)).toLocaleTimeString('zh-CN', {
                    hour: '2-digit',
                    minute: '2-digit',
                    ...(span < 120000 ? { second: '2-digit' } : {}),
                  }),
            title: false,
          },
          y: { labelFormatter: (value: number) => format(Number(value)), title: false },
        }}
        tooltip={{
          title: (datum: { at: number }) => new Date(datum.at).toLocaleString('zh-CN'),
          items: [{ channel: 'y', valueFormatter: (value: number) => format(value) }],
        }}
        legend={{ color: { position: 'top', itemLabelFill: token.colorText } }}
        theme={token.colorBgContainer === '#141414' ? 'dark' : 'light'}
        animate={false}
      />
    </div>
  );
}
