import { Card, Col, Descriptions, Row, Space } from 'antd';
import type { Overview, TrafficPoint } from '../types';
import { aggregate, bytes } from '../lib/format';
import MetricCard from '../components/MetricCard';
import TrafficChart from '../components/TrafficChart';

export default function PerformancePage({
  overview,
  history,
}: {
  overview: Overview;
  history: TrafficPoint[];
}) {
  const totals = aggregate(overview),
    process = overview.process;
  return (
    <Space orientation="vertical" size={24} className="full-width">
      <Row gutter={[16, 16]}>
        {[
          ['TCP 上传', totals.tcpUp],
          ['TCP 下载', totals.tcpDown],
          ['UDP 上传', totals.udpUp],
          ['UDP 下载', totals.udpDown],
          ['HTTP 上传', totals.httpUp],
          ['HTTP 下载', totals.httpDown],
        ].map(([title, value]) => (
          <Col xs={12} lg={8} xl={4} key={title}>
            <MetricCard title={String(title)} value={bytes(Number(value))} />
          </Col>
        ))}
      </Row>
      <Card className="chart-card" title="实时吞吐趋势" extra="最近 5 分钟 · 自动同步">
        <TrafficChart
          points={history.map((point) => ({ ...point }))}
          fields={[
            { key: 'upload', label: '上传' },
            { key: 'download', label: '下载' },
          ]}
          unit="rate"
        />
      </Card>
      <Card title="进程与协议指标">
        <Descriptions
          bordered
          column={{ xs: 1, sm: 1, md: 2, xl: 3 }}
          items={[
            {
              key: 'heap',
              label: 'GC 存活堆',
              children: bytes(process.heap_live_bytes || process.heap_bytes),
            },
            { key: 'alloc', label: '当前分配堆', children: bytes(process.heap_bytes) },
            { key: 'sys', label: 'Go 系统内存', children: bytes(process.sys_bytes) },
            { key: 'goroutines', label: 'Goroutine', children: process.goroutines },
            { key: 'gc', label: 'GC 次数', children: process.gc_cycles },
            { key: 'ws', label: 'WebSocket 客户端', children: process.websocket_clients },
            { key: 'active', label: '活跃会话', children: totals.active },
            { key: 'errors', label: '连接 / 请求错误', children: totals.errors },
            { key: 'packets', label: 'UDP 数据包', children: totals.udpPackets },
            { key: 'drops', label: 'UDP 丢弃 / 错误', children: totals.udpErrors },
          ]}
        />
      </Card>
    </Space>
  );
}
