import { useState } from 'react';
import { Alert, Button, Card, Col, Row, Segmented, Select, Space, Spin } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import type { ServerConfig, StatsPoint, StatsSummary } from '../types';
import { useQuery } from '../hooks/useQuery';
import { bytes } from '../lib/format';
import MetricCard from '../components/MetricCard';
import TrafficChart from '../components/TrafficChart';

export default function StatisticsPage({ servers }: { servers: ServerConfig[] }) {
  const [server, setServer] = useState(0),
    [range, setRange] = useState('24h'),
    [end, setEnd] = useState(() => Date.now());
  const query = new URLSearchParams({
    from: new Date(end - (range === '7d' ? 7 : 1) * 86400000).toISOString(),
    to: new Date(end).toISOString(),
    step: range === '7d' ? 'hour' : 'minute',
  });
  if (server) query.set('server_id', String(server));
  const points = useQuery<StatsPoint[]>('/api/stats?' + query),
    summary = useQuery<StatsSummary>('/api/stats/summary?' + query);
  const values = (points.data || []).map((point) => ({
    ...point,
    at: Date.parse(point.bucket),
    heartbeat_healthy: Number(point.heartbeat_healthy),
  }));
  const loading = points.loading || summary.loading,
    error = points.error || summary.error,
    sum = summary.data;
  return (
    <Space orientation="vertical" size={24} className="full-width">
      <Card>
        <div className="statistics-toolbar">
          <Select
            aria-label="统计出口"
            value={server}
            onChange={(value) => {
              setServer(value);
              setEnd(Date.now());
            }}
            className="statistics-select"
            options={[
              { value: 0, label: '全部出口' },
              ...servers.map((server) => ({ value: server.id!, label: server.name })),
            ]}
          />
          <Segmented
            value={range}
            onChange={(value) => {
              setRange(String(value));
              setEnd(Date.now());
            }}
            options={[
              { value: '24h', label: '最近 24 小时' },
              { value: '7d', label: '最近 7 天' },
            ]}
          />
          <Button
            icon={<ReloadOutlined />}
            loading={loading}
            onClick={() => {
              setEnd(Date.now());
              points.refresh();
              summary.refresh();
            }}
          >
            刷新统计
          </Button>
        </div>
      </Card>
      {error && <Alert type="error" showIcon title={error} />}
      <Row gutter={[16, 16]}>
        <Col xs={12} xl={6}>
          <MetricCard title="上传流量" value={sum ? bytes(sum.upload_bytes) : '—'} />
        </Col>
        <Col xs={12} xl={6}>
          <MetricCard title="下载流量" value={sum ? bytes(sum.download_bytes) : '—'} />
        </Col>
        <Col xs={12} xl={6}>
          <MetricCard
            title="平均心跳延迟"
            value={sum?.avg_latency_ms ? Math.round(sum.avg_latency_ms * 10) / 10 + ' ms' : '—'}
          />
        </Col>
        <Col xs={12} xl={6}>
          <MetricCard
            title="成功率"
            value={sum ? (sum.success_rate * 100).toFixed(2) + '%' : '—'}
          />
        </Col>
      </Row>
      <Spin spinning={loading}>
        <Card
          className="chart-card"
          title="历史流量趋势"
          extra={range === '7d' ? '每小时汇总 · 保留 7 天' : '每分钟汇总'}
        >
          <TrafficChart
            points={values}
            fields={[
              { key: 'upload_bytes', label: '上传流量' },
              { key: 'download_bytes', label: '下载流量' },
            ]}
          />
        </Card>
      </Spin>
      <Row gutter={[16, 16]}>
        <Col xs={24} xl={12}>
          <Card title="心跳延迟">
            <TrafficChart
              points={values}
              fields={[{ key: 'heartbeat_latency_ms', label: '心跳延迟' }]}
              unit="latency"
            />
          </Card>
        </Col>
        <Col xs={24} xl={12}>
          <Card title="连接成功率">
            <TrafficChart
              points={values}
              fields={[{ key: 'success_rate', label: '成功率' }]}
              unit="percent"
            />
          </Card>
        </Col>
      </Row>
    </Space>
  );
}
