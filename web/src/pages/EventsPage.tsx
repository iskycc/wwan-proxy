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
import type { VohiveEvent } from '../types';
import { time } from '../lib/format';
import { eventColors, eventLabels, eventSummary } from '../lib/vohive';
import DetailView from '../components/DetailView';

export default function EventsPage() {
  const [type, setType] = useState(''),
    [device, setDevice] = useState('');
  const compact = !Grid.useBreakpoint().md;
  const result = useQuery<VohiveEvent[]>(
    '/api/vohive/events?' + new URLSearchParams({ limit: '300', type, device }),
  );
  const description = (row: VohiveEvent) => {
    const summary = eventSummary(row);
    return (
      <Space orientation="vertical" size={4} className="full-width">
        <span className="break-text">{summary.message}</span>
        <Space wrap size={[4, 4]} className="full-width">
          {summary.chips.map((chip) => (
            <Tag className="event-chip" key={chip.text} color={chip.color}>
              {chip.text}
            </Tag>
          ))}
        </Space>
      </Space>
    );
  };
  return (
    <Card
      title="Vohive 设备事件"
      extra={
        <Button
          aria-label="刷新事件"
          icon={<ReloadOutlined />}
          loading={result.loading}
          onClick={result.refresh}
        >
          刷新
        </Button>
      }
    >
      <Typography.Paragraph type="secondary">
        查看设备健康变化、网络重启及自动恢复结果。展开事件可查看完整诊断。
      </Typography.Paragraph>
      <div className="filter-bar">
        <Select
          aria-label="事件类型"
          value={type}
          onChange={setType}
          className="filter-select"
          options={[
            { value: '', label: '全部类型' },
            ...Object.entries(eventLabels).map(([value, label]) => ({ value, label })),
          ]}
        />
        <Input.Search
          placeholder="筛选设备编码"
          allowClear
          onSearch={setDevice}
          className="filter-search"
          aria-label="筛选设备编码"
        />
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
                <Typography.Text type="secondary">{time(row.created_at)}</Typography.Text>
                <Space wrap size={[4, 4]}>
                  <Tag>{row.device_id || '系统'}</Tag>
                  <Tag color={eventColors[row.type]}>{eventLabels[row.type] || row.type}</Tag>
                </Space>
                {description(row)}
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
        <Table<VohiveEvent>
          rowKey="id"
          loading={result.loading}
          dataSource={result.data || []}
          scroll={{ x: 800 }}
          pagination={{ defaultPageSize: 20, showSizeChanger: true }}
          expandable={{
            expandedRowRender: (row) => (
              <DetailView value={{ message: row.message, ...row.details }} />
            ),
          }}
          columns={[
            { title: '时间', dataIndex: 'created_at', width: 185, render: time },
            {
              title: '设备',
              dataIndex: 'device_id',
              width: 110,
              render: (value) => <Tag>{value || '系统'}</Tag>,
            },
            {
              title: '事件',
              dataIndex: 'type',
              width: 110,
              render: (value) => (
                <Tag color={eventColors[value]}>{eventLabels[value] || value}</Tag>
              ),
            },
            {
              title: '事件说明',
              render: (_, row) => description(row),
            },
          ]}
        />
      )}
    </Card>
  );
}
