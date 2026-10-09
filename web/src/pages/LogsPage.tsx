import { useState } from 'react';
import {
  Alert,
  Button,
  Card,
  Collapse,
  Grid,
  Input,
  List,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import { useQuery } from '../hooks/useQuery';
import type { LogEntry } from '../types';
import { time } from '../lib/format';
import DetailView from '../components/DetailView';

export default function LogsPage() {
  const [level, setLevel] = useState(''),
    [query, setQuery] = useState('');
  const compact = !Grid.useBreakpoint().md;
  const result = useQuery<LogEntry[]>(
    '/api/logs?' + new URLSearchParams({ limit: '300', level, q: query }),
  );
  return (
    <Card
      className="records-card"
      title="运行日志"
      extra={
        <Button
          aria-label="刷新日志"
          icon={<ReloadOutlined />}
          loading={result.loading}
          onClick={result.refresh}
        >
          刷新
        </Button>
      }
    >
      <div className="filter-bar">
        <Select
          aria-label="日志级别"
          value={level}
          onChange={setLevel}
          className="filter-select"
          options={['', 'DEBUG', 'INFO', 'WARN', 'ERROR'].map((value) => ({
            value,
            label: value || '全部级别',
          }))}
        />
        <Input.Search
          placeholder="搜索消息、实例或错误"
          allowClear
          onSearch={setQuery}
          className="filter-search"
          aria-label="搜索运行日志"
        />
        <Typography.Text type="secondary" className="filter-note">
          显示最近 300 条记录
        </Typography.Text>
      </div>
      {result.error && <Alert type="error" showIcon title={result.error} className="form-alert" />}
      {compact ? (
        <List
          className="mobile-records"
          loading={result.loading}
          dataSource={result.data || []}
          pagination={{ defaultPageSize: 20, size: 'small', showSizeChanger: false }}
          renderItem={(row) => (
            <List.Item>
              <div className="mobile-record">
                <Space wrap size={[4, 4]}>
                  <Tag
                    color={row.level === 'ERROR' ? 'red' : row.level === 'WARN' ? 'orange' : 'blue'}
                  >
                    {row.level}
                  </Tag>
                  <Typography.Text type="secondary" className="record-time">
                    {time(row.timestamp)}
                  </Typography.Text>
                </Space>
                <Typography.Text strong ellipsis={{ tooltip: row.server_name || row.component }}>
                  {row.server_name || row.component || '系统'}
                </Typography.Text>
                <Typography.Paragraph className="record-message" ellipsis={{ rows: 3 }}>
                  {row.message}
                </Typography.Paragraph>
                <Collapse
                  ghost
                  size="small"
                  items={[
                    {
                      key: 'details',
                      label: '查看详情',
                      children: <DetailView value={{ message: row.message, ...row.details }} />,
                    },
                  ]}
                />
              </div>
            </List.Item>
          )}
        />
      ) : (
        <Table<LogEntry>
          tableLayout="fixed"
          rowKey="id"
          loading={result.loading}
          dataSource={result.data || []}
          scroll={{ x: 800 }}
          pagination={{
            defaultPageSize: 20,
            showSizeChanger: true,
            showTotal: (total) => '共 ' + total + ' 条记录',
          }}
          expandable={{
            expandedRowRender: (row) => (
              <DetailView value={{ message: row.message, ...row.details }} />
            ),
          }}
          columns={[
            {
              title: '时间',
              dataIndex: 'timestamp',
              width: 180,
              render: (value) => <span className="record-time">{time(value)}</span>,
            },
            {
              title: '级别',
              dataIndex: 'level',
              width: 85,
              render: (value) => (
                <Tag
                  color={
                    value === 'ERROR'
                      ? 'red'
                      : value === 'WARN'
                        ? 'orange'
                        : value === 'INFO'
                          ? 'blue'
                          : 'default'
                  }
                >
                  {value}
                </Tag>
              ),
            },
            {
              title: '实例 / 组件',
              width: 170,
              render: (_, row) => (
                <Space orientation="vertical" size={0} className="full-width">
                  <Typography.Text ellipsis={{ tooltip: row.server_name }}>
                    {row.server_name || '系统'}
                  </Typography.Text>
                  <Typography.Text type="secondary" className="break-text record-component">
                    {row.component}
                  </Typography.Text>
                </Space>
              ),
            },
            {
              title: '消息',
              dataIndex: 'message',
              render: (value) => (
                <Typography.Paragraph className="record-message" ellipsis={{ rows: 3 }}>
                  {value}
                </Typography.Paragraph>
              ),
            },
          ]}
        />
      )}
    </Card>
  );
}
