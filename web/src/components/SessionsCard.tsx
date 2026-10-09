import { Alert, App, Button, Card, List, Space, Tag, Typography } from 'antd';
import { DesktopOutlined, ReloadOutlined } from '@ant-design/icons';
import { useQuery } from '../hooks/useQuery';
import type { Session } from '../types';
import { api, errorMessage } from '../lib/api';
import { time } from '../lib/format';

export default function SessionsCard({ onExpired }: { onExpired: () => void }) {
  const result = useQuery<Session[]>('/api/sessions'),
    { message, modal } = App.useApp();
  const revoke = async (session: Session) =>
    await modal.confirm({
      title: session.current ? '退出当前会话' : '撤销登录设备',
      content: '此设备需要重新登录才能访问控制台。',
      onOk: async () => {
        try {
          await api('/api/sessions/' + session.id, { method: 'DELETE' });
          if (session.current) onExpired();
          else {
            message.success('登录设备已撤销');
            result.refresh();
          }
        } catch (error) {
          message.error(errorMessage(error));
          throw error;
        }
      },
    });
  const revokeOthers = async () =>
    await modal.confirm({
      title: '撤销其他登录设备',
      content: '保留当前浏览器，其他设备将立即失去访问权限。',
      onOk: async () => {
        try {
          await api('/api/sessions/revoke-others', { method: 'POST' });
          message.success('其他会话已撤销');
          result.refresh();
        } catch (error) {
          message.error(errorMessage(error));
          throw error;
        }
      },
    });
  return (
    <Card
      className="sessions-card"
      title="登录设备"
      extra={
        <Space>
          <Button
            icon={<ReloadOutlined />}
            aria-label="刷新登录设备"
            loading={result.loading}
            onClick={result.refresh}
          />
          <Button onClick={revokeOthers}>撤销其他设备</Button>
        </Space>
      }
    >
      {result.error && <Alert type="error" title={result.error} className="form-alert" />}
      <List
        loading={result.loading}
        dataSource={result.data || []}
        renderItem={(session) => (
          <List.Item
            key={session.id}
            actions={[
              <Button key="revoke" danger size="small" onClick={() => revoke(session)}>
                {session.current ? '退出当前' : '撤销'}
              </Button>,
            ]}
          >
            <List.Item.Meta
              avatar={<DesktopOutlined className="session-icon" />}
              title={
                <Space>
                  {session.current ? '当前浏览器' : '已登录设备'}
                  {session.current && <Tag color="blue">当前</Tag>}
                </Space>
              }
              description={
                <div className="session-description">
                  <Typography.Paragraph
                    className="session-agent"
                    type="secondary"
                    ellipsis={{
                      rows: 2,
                      expandable: 'collapsible',
                      symbol: (expanded) => (expanded ? '收起' : '展开客户端信息'),
                    }}
                  >
                    {session.user_agent || '未知客户端'}
                  </Typography.Paragraph>
                  <span>来源：{session.remote_addr || '—'}</span>
                  <span>
                    登录：{time(session.created_at)} · 到期：{time(session.expires_at)}
                  </span>
                </div>
              }
            />
          </List.Item>
        )}
      />
    </Card>
  );
}
