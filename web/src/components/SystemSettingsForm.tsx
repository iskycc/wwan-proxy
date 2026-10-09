import { useEffect, useState } from 'react';
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Form,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
  Switch,
  Tag,
  Typography,
} from 'antd';
import { SaveOutlined } from '@ant-design/icons';
import { api, errorMessage, jsonBody } from '../lib/api';
import type { SystemSettings } from '../types';

export default function SystemSettingsForm({
  settings,
  onSaved,
}: {
  settings: SystemSettings;
  onSaved: (settings: SystemSettings) => Promise<void>;
}) {
  const [form] = Form.useForm<SystemSettings>(),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const enabled = Form.useWatch(['vohive', 'enabled'], form);
  const { message } = App.useApp();
  useEffect(() => {
    form.setFieldsValue({
      web_listen: settings.web_listen,
      database_path: settings.database_path,
      log_level: settings.log_level,
      log_retention_days: settings.log_retention_days,
      session_lifetime: settings.session_lifetime,
      vohive: { ...settings.vohive },
    });
  }, [settings, form]);
  const save = async () => {
    setBusy(true);
    setError('');
    try {
      const value = form.getFieldsValue(true);
      const result = await api<SystemSettings>('/api/settings', {
        method: 'PUT',
        ...jsonBody(value),
      });
      await onSaved(result);
      message.success(
        result.restart_required ? '设置已保存，部分项目重启后生效' : '系统设置已生效',
      );
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card
      title="系统与 Vohive 设置"
      extra={
        <Tag color={settings.restart_required ? 'orange' : 'green'}>
          {settings.restart_required ? '等待重启' : '已同步'}
        </Tag>
      }
    >
      <Typography.Paragraph type="secondary">
        日志与会话策略保存后立即生效；监听地址、数据库位置和 Vohive 设置需要重启服务。
      </Typography.Paragraph>
      {settings.restart_required && (
        <Alert
          className="form-alert"
          type="warning"
          showIcon
          title="部分设置已变更，请安全重启 wwan-proxy 服务。"
        />
      )}
      {error && <Alert className="form-alert" type="error" showIcon title={error} />}
      <Form form={form} layout="vertical" onFinish={save} disabled={busy}>
        <Form.Item
          name="web_listen"
          label="管理页面监听地址"
          rules={[{ required: true, message: '请输入监听地址' }]}
          extra={'当前监听 ' + (settings.current_web_listen || settings.web_listen)}
        >
          <Input placeholder="127.0.0.1:9090" />
        </Form.Item>
        <Form.Item
          name="database_path"
          label="数据库路径"
          rules={[{ required: true, message: '请输入绝对路径' }]}
          extra={'当前数据库 ' + (settings.current_database_path || settings.database_path)}
        >
          <Input placeholder="/var/lib/wwan-proxy/wwan-proxy.db" />
        </Form.Item>
        <Row gutter={20}>
          <Col xs={24} md={12}>
            <Form.Item name="log_level" label="最低日志级别">
              <Select
                options={['DEBUG', 'INFO', 'WARN', 'ERROR'].map((value) => ({
                  value,
                  label: value,
                }))}
              />
            </Form.Item>
          </Col>
          <Col xs={24} md={12}>
            <Form.Item name="log_retention_days" label="日志保留天数" rules={[{ required: true }]}>
              <InputNumber min={1} max={3650} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
        </Row>
        <Form.Item
          name="session_lifetime"
          label="登录会话有效期"
          rules={[{ required: true, message: '请输入有效期' }]}
          extra="范围为 5m–720h，已过期的会话会立即退出。"
        >
          <Input placeholder="24h" />
        </Form.Item>
        <div className="settings-divider">
          <Typography.Title level={5}>Vohive 设备自动恢复</Typography.Title>
          <Typography.Paragraph type="secondary">
            出口心跳连续失败时，通过 Vohive API 恢复指定设备的移动数据网络。
          </Typography.Paragraph>
        </div>
        <Form.Item name={['vohive', 'enabled']} label="启用 Vohive" valuePropName="checked">
          <Switch />
        </Form.Item>
        {enabled && (
          <>
            <Form.Item
              name={['vohive', 'base_url']}
              label="Vohive API 地址"
              rules={[
                { required: true, message: '请输入 API 地址' },
                { type: 'url', message: '请输入完整的 HTTP / HTTPS URL' },
              ]}
            >
              <Input placeholder="http://192.168.8.1:8080" />
            </Form.Item>
            <Row gutter={20}>
              <Col xs={24} md={12}>
                <Form.Item
                  name={['vohive', 'username']}
                  label="Vohive 用户名"
                  rules={[{ required: true }]}
                >
                  <Input autoComplete="off" />
                </Form.Item>
              </Col>
              <Col xs={24} md={12}>
                <Form.Item
                  name={['vohive', 'password']}
                  label="Vohive 密码"
                  rules={[{ required: true }]}
                >
                  <Input.Password autoComplete="new-password" />
                </Form.Item>
              </Col>
            </Row>
            <Row gutter={20}>
              <Col xs={24} md={12}>
                <Form.Item
                  name={['vohive', 'consecutive_failures']}
                  label="连续失败阈值"
                  rules={[{ required: true }]}
                >
                  <InputNumber min={1} max={100} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              <Col xs={24} md={12}>
                <Form.Item
                  name={['vohive', 'cooldown']}
                  label="恢复冷却时间"
                  rules={[{ required: true }]}
                >
                  <Input placeholder="5m" />
                </Form.Item>
              </Col>
            </Row>
          </>
        )}
        <Space>
          <Button type="primary" htmlType="submit" loading={busy} icon={<SaveOutlined />}>
            保存系统设置
          </Button>
        </Space>
      </Form>
    </Card>
  );
}
