import {
  Alert,
  Badge,
  Button,
  Card,
  Col,
  Descriptions,
  Empty,
  Row,
  Space,
  Tag,
  Typography,
} from 'antd';
import {
  ApiOutlined,
  ArrowUpOutlined,
  EditOutlined,
  LinkOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import type { Overview, ServerConfig } from '../types';
import { aggregate, bytes, duration, serverHealth, time } from '../lib/format';
import MetricCard from '../components/MetricCard';

export default function OverviewPage({
  overview,
  onEdit,
}: {
  overview: Overview;
  onEdit: (config: ServerConfig) => void;
}) {
  const totals = aggregate(overview),
    enabled = overview.servers.filter((server) => server.enabled).length;
  const healthy = overview.servers.filter(
    (server) => serverHealth(server, overview).status === 'success',
  ).length;
  return (
    <Space orientation="vertical" size={24} className="full-width">
      <Card className="overview-banner">
        <Row gutter={[24, 20]} align="middle">
          <Col xs={24} lg={17}>
            <Space>
              <Badge status={!enabled ? 'default' : healthy === enabled ? 'success' : 'warning'} />
              <Typography.Text strong>
                {!enabled
                  ? '等待启用出口'
                  : healthy === enabled
                    ? '所有出口运行正常'
                    : '部分出口需要关注'}
              </Typography.Text>
            </Space>
            <Typography.Title level={2}>你的网络，尽在掌控。</Typography.Title>
            <Typography.Paragraph type="secondary">
              查看代理实例的链路健康、会话与流量，随时调整每个出口的连接策略。
            </Typography.Paragraph>
            <Space wrap>
              <Tag icon={<ApiOutlined />}>SOCKS5</Tag>
              <Tag>HTTP / HTTPS</Tag>
              <Tag>原生 UDP</Tag>
              <Tag>DNS / DoH</Tag>
            </Space>
          </Col>
          <Col xs={24} lg={7}>
            <div className="health-summary">
              <span className="health-count">
                {healthy}
                <small> / {enabled}</small>
              </span>
              <Typography.Text type="secondary">
                健康出口 · 已运行 {duration(overview.uptime_seconds)}
              </Typography.Text>
            </div>
          </Col>
        </Row>
      </Card>
      <Row gutter={[16, 16]}>
        <Col xs={12} xl={6}>
          <MetricCard
            title="活跃会话"
            value={totals.active}
            icon={<LinkOutlined />}
            note="SOCKS5 · HTTP · UDP"
          />
        </Col>
        <Col xs={12} xl={6}>
          <MetricCard title="累计连接 / 请求" value={totals.total} icon={<ThunderboltOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <MetricCard
            title="累计传输流量"
            value={bytes(
              totals.tcpUp +
                totals.tcpDown +
                totals.udpUp +
                totals.udpDown +
                totals.httpUp +
                totals.httpDown,
            )}
            icon={<ArrowUpOutlined />}
          />
        </Col>
        <Col xs={12} xl={6}>
          <MetricCard
            title="进程内存"
            value={bytes(overview.process.heap_live_bytes || overview.process.heap_bytes)}
            note={overview.process.goroutines + ' 个 goroutine'}
          />
        </Col>
      </Row>
      <div className="section-title">
        <Typography.Title level={4}>出口状态</Typography.Title>
        <Typography.Text type="secondary">{overview.servers.length} 个代理实例</Typography.Text>
      </div>
      {!overview.servers.length ? (
        <Card>
          <Empty description="尚未创建出口，请点击右上角「新建出口」。" />
        </Card>
      ) : (
        <Row gutter={[16, 16]}>
          {overview.servers.map((server) => {
            const status = serverHealth(server, overview),
              hb = overview.heartbeats[server.id!],
              instance = overview.instances.find((value) => value.id === server.id);
            return (
              <Col xs={24} md={12} xxl={8} key={server.id}>
                <Card
                  className="egress-card"
                  title={
                    <div className="egress-title">
                      <span className="egress-icon">
                        <ApiOutlined />
                      </span>
                      <Typography.Text ellipsis={{ tooltip: server.name }}>
                        {server.name}
                      </Typography.Text>
                    </div>
                  }
                  extra={
                    <Button
                      type="text"
                      aria-label={'编辑 ' + server.name}
                      icon={<EditOutlined />}
                      onClick={() => onEdit(server)}
                    />
                  }
                >
                  <div className="egress-status">
                    <Tag>{server.interface}</Tag>
                    <Badge status={status.status} text={status.text} />
                  </div>
                  <Descriptions
                    column={1}
                    size="small"
                    items={[
                      {
                        key: 'ip',
                        label: '公网 IP',
                        children: (
                          <Typography.Text className="break-text" copyable={!!hb?.public_ip}>
                            {hb?.public_ip || '—'}
                          </Typography.Text>
                        ),
                      },
                      {
                        key: 'latency',
                        label: '心跳延迟',
                        children: hb?.healthy ? hb.latency_ms + ' ms' : '—',
                      },
                      { key: 'colo', label: 'Cloudflare POP', children: hb?.colo || '—' },
                      {
                        key: 'socks',
                        label: 'SOCKS5',
                        children: (
                          <Typography.Text className="break-text" code>
                            {server.listen}
                          </Typography.Text>
                        ),
                      },
                      ...(server.http_proxy.enabled
                        ? [
                            {
                              key: 'http',
                              label: 'HTTP',
                              children: (
                                <Typography.Text className="break-text" code>
                                  {server.http_proxy.listen}
                                </Typography.Text>
                              ),
                            },
                          ]
                        : []),
                    ]}
                  />
                  {(hb?.error || instance?.last_error) && (
                    <Alert
                      type="error"
                      title={
                        <Typography.Paragraph
                          className="error-summary"
                          ellipsis={{
                            rows: 3,
                            expandable: 'collapsible',
                            symbol: (expanded) => (expanded ? '收起' : '展开'),
                          }}
                        >
                          {hb?.error || instance?.last_error}
                        </Typography.Paragraph>
                      }
                      className="egress-error"
                    />
                  )}
                  <Typography.Text type="secondary" className="egress-time">
                    最近检查 {time(hb?.checked_at)}
                  </Typography.Text>
                </Card>
              </Col>
            );
          })}
        </Row>
      )}
    </Space>
  );
}
