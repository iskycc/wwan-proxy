import { Alert, Button, Card, Col, Empty, Row, Space, Spin } from 'antd';
import type { ServerConfig, SystemSettings } from '../types';
import { useQuery } from '../hooks/useQuery';
import SystemSettingsForm from '../components/SystemSettingsForm';
import AdminSettingsForm from '../components/AdminSettingsForm';
import SessionsCard from '../components/SessionsCard';
import UpdateCard from '../components/UpdateCard';

export default function SettingsPage({
  servers,
  username,
  refreshSession,
  onExpired,
}: {
  servers: ServerConfig[];
  username: string;
  refreshSession: () => Promise<unknown>;
  onExpired: () => void;
}) {
  const result = useQuery<SystemSettings>('/api/settings');
  return (
    <Space orientation="vertical" size={24} className="full-width">
      {result.error && (
        <Alert
          type="error"
          showIcon
          title={result.error}
          action={
            <Button aria-label="重试系统设置" onClick={result.refresh}>
              重试
            </Button>
          }
        />
      )}
      <Row gutter={[24, 24]} align="top">
        <Col xs={24} xl={14}>
          {result.data ? (
            <SystemSettingsForm
              settings={result.data}
              onSaved={async (value) => {
                result.setData(value);
                await refreshSession();
              }}
            />
          ) : (
            <Card title="系统与 Vohive 设置">
              <div className="loading-panel">
                {result.loading ? (
                  <Space orientation="vertical" size={16}>
                    <Spin />
                    <span>读取系统设置…</span>
                  </Space>
                ) : (
                  <Empty description="尚未加载系统设置，请点击上方重试。" />
                )}
              </div>
            </Card>
          )}
        </Col>
        <Col xs={24} xl={10}>
          <Space orientation="vertical" size={24} className="full-width">
            <AdminSettingsForm username={username} onSaved={refreshSession} />
            <UpdateCard servers={servers} />
          </Space>
        </Col>
      </Row>
      <SessionsCard onExpired={onExpired} />
    </Space>
  );
}
