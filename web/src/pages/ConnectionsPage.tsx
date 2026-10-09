import { useMemo, useState } from 'react';
import { App, Badge, Button, Card, Grid, Input, List, Space, Table, Tag, Typography } from 'antd';
import { EditOutlined, SearchOutlined } from '@ant-design/icons';
import type { Overview, ServerConfig } from '../types';
import { serverHealth } from '../lib/format';
import { api, errorMessage } from '../lib/api';

export default function ConnectionsPage({
  overview,
  onEdit,
  onChanged,
}: {
  overview: Overview;
  onEdit: (server: ServerConfig) => void;
  onChanged: () => void;
}) {
  const [query, setQuery] = useState(''),
    [busy, setBusy] = useState<number | null>(null);
  const { message } = App.useApp();
  const compact = !Grid.useBreakpoint().md;
  const rows = useMemo(
    () =>
      overview.servers.filter((server) =>
        (server.name + ' ' + server.interface + ' ' + server.listen)
          .toLowerCase()
          .includes(query.toLowerCase()),
      ),
    [overview.servers, query],
  );
  const toggle = async (server: ServerConfig) => {
    setBusy(server.id!);
    try {
      await api('/api/servers/' + server.id + '/toggle', { method: 'POST' });
      message.success(server.enabled ? '实例已停用' : '实例已启用');
      onChanged();
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setBusy(null);
    }
  };
  const actions = (server: ServerConfig) => (
    <Space wrap>
      <Button
        size={compact ? 'middle' : 'small'}
        aria-label={server.enabled ? '停用' : '启用'}
        loading={busy === server.id}
        onClick={() => toggle(server)}
      >
        {server.enabled ? '停用' : '启用'}
      </Button>
      <Button
        size={compact ? 'middle' : 'small'}
        aria-label="编辑"
        icon={<EditOutlined />}
        onClick={() => onEdit(server)}
      >
        编辑
      </Button>
    </Space>
  );
  return (
    <Card title="代理实例" className="records-card">
      <Typography.Paragraph type="secondary" className="section-intro">
        配置保存后自动热应用。每个实例使用独立的出口网口和访问策略。
      </Typography.Paragraph>
      <div className="table-toolbar">
        <Input
          aria-label="搜索代理实例"
          prefix={<SearchOutlined />}
          placeholder="搜索名称、接口或地址"
          allowClear
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          className="table-search"
        />
        <Typography.Text type="secondary" className="table-count">
          {query ? '匹配 ' + rows.length + ' / ' : '共 '}
          {overview.servers.length} 个实例
        </Typography.Text>
      </div>
      {compact ? (
        <List
          className="mobile-records"
          dataSource={rows}
          pagination={{ defaultPageSize: 10, size: 'small', showSizeChanger: false }}
          renderItem={(server) => {
            const status = serverHealth(server, overview);
            return (
              <List.Item>
                <div className="mobile-record">
                  <Typography.Paragraph strong ellipsis={{ rows: 2, tooltip: server.name }}>
                    {server.name}
                  </Typography.Paragraph>
                  <Space wrap size={[4, 8]}>
                    <Tag color="blue">{server.interface}</Tag>
                    <Badge status={status.status} text={status.text} />
                    <Tag>
                      {server.dns.doh ? 'DoH' : server.dns.servers?.length ? 'DNS' : '系统 DNS'}
                    </Tag>
                    {server.udp.enabled && <Tag>UDP</Tag>}
                  </Space>
                  <div className="record-addresses">
                    <Typography.Text className="endpoint-text">
                      SOCKS5：{server.listen}
                    </Typography.Text>
                    {server.http_proxy.enabled && (
                      <Typography.Text className="endpoint-text">
                        HTTP：{server.http_proxy.listen}
                      </Typography.Text>
                    )}
                  </div>
                  {actions(server)}
                </div>
              </List.Item>
            );
          }}
        />
      ) : (
        <Table<ServerConfig>
          tableLayout="fixed"
          rowKey="id"
          dataSource={rows}
          scroll={{ x: 1088 }}
          pagination={{
            defaultPageSize: 10,
            showSizeChanger: true,
            showTotal: (total) => '共 ' + total + ' 个实例',
          }}
          columns={[
            {
              title: '名称',
              dataIndex: 'name',
              fixed: 'left',
              width: 170,
              render: (value) => (
                <Typography.Paragraph
                  className="connection-name"
                  strong
                  ellipsis={{ rows: 2, tooltip: value }}
                >
                  {value}
                </Typography.Paragraph>
              ),
            },
            {
              title: '监听地址',
              width: 275,
              render: (_, server) => (
                <Space orientation="vertical" size={6} className="full-width">
                  <span className="listener-address">
                    <Tag>SOCKS5</Tag>
                    <span className="endpoint-text">{server.listen}</span>
                  </span>
                  {server.http_proxy.enabled && (
                    <span className="listener-address">
                      <Tag>HTTP</Tag>
                      <span className="endpoint-text">{server.http_proxy.listen}</span>
                    </span>
                  )}
                </Space>
              ),
            },
            {
              title: '出口网口',
              width: 128,
              dataIndex: 'interface',
              render: (value) => <Tag color="blue">{value}</Tag>,
            },
            {
              title: 'DNS',
              width: 116,
              render: (_, server) => (
                <Space wrap size={4}>
                  <Tag>{server.dns.doh ? 'DoH' : server.dns.servers?.length ? 'DNS' : '系统'}</Tag>
                  {server.dns.ipv4_only && <Tag>IPv4</Tag>}
                </Space>
              ),
            },
            {
              title: 'UDP',
              width: 125,
              render: (_, server) =>
                !server.udp.enabled ? (
                  <Typography.Text type="secondary">关闭</Typography.Text>
                ) : server.udp.relay_ports?.length || server.udp.relay_port ? (
                  <Tag color="cyan">固定端口池</Tag>
                ) : (
                  <span>
                    {server.udp.port_min}–{server.udp.port_max}
                  </span>
                ),
            },
            {
              title: '状态',
              width: 104,
              render: (_, server) => {
                const status = serverHealth(server, overview);
                return <Badge status={status.status} text={status.text} />;
              },
            },
            {
              title: '操作',
              fixed: 'right',
              width: 170,
              render: (_, server) => actions(server),
            },
          ]}
        />
      )}
    </Card>
  );
}
