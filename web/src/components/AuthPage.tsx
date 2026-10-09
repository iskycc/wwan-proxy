import { useState } from 'react';
import { Alert, Button, Card, Form, Input, Progress, Space, Typography } from 'antd';
import { LockOutlined, SafetyCertificateOutlined, UserOutlined } from '@ant-design/icons';
import { api, errorMessage, jsonBody } from '../lib/api';
import type { AuthStatus } from '../types';

export default function AuthPage({
  initialized,
  onAuthenticated,
}: {
  initialized: boolean;
  onAuthenticated: (session: AuthStatus) => void;
}) {
  const [loading, setLoading] = useState(false),
    [error, setError] = useState('');
  const [form] = Form.useForm();
  const password = Form.useWatch('password', form) || '';
  const passwordRule = {
    validator: (_: unknown, value: string) => {
      const length = new TextEncoder().encode(value || '').length;
      return length >= 12 && length <= 72
        ? Promise.resolve()
        : Promise.reject(new Error('密码长度必须为 12–72 字节'));
    },
  };
  const submit = async (value: { username: string; password: string }) => {
    setLoading(true);
    setError('');
    try {
      const result = await api<AuthStatus>(
        initialized ? '/api/auth/login' : '/api/auth/initialize',
        {
          method: 'POST',
          ...jsonBody({ username: value.username.trim(), password: value.password }),
        },
      );
      onAuthenticated({ ...result, initialized: true });
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setLoading(false);
    }
  };
  return (
    <main className="auth-layout">
      <section className="auth-intro">
        <div className="brand-mark">W</div>
        <Typography.Text className="auth-eyebrow">WWAN CONTROL CENTER</Typography.Text>
        <Typography.Title>
          连接每一个出口，
          <br />
          掌握每一条链路。
        </Typography.Title>
        <Typography.Paragraph>
          多出口代理、实时监控与设备恢复，统一在你的网络控制中心。
        </Typography.Paragraph>
        <Space wrap>
          <span className="auth-feature">
            <SafetyCertificateOutlined /> 本地账户
          </span>
          <span className="auth-feature">独立出口</span>
          <span className="auth-feature">实时状态</span>
        </Space>
      </section>
      <Card className="auth-card">
        <Space className="auth-card-brand">
          <div className="brand-mark small">W</div>
          <Typography.Text strong>WWAN Control</Typography.Text>
        </Space>
        <Typography.Title level={2}>{initialized ? '欢迎回来' : '创建管理员账户'}</Typography.Title>
        <Typography.Paragraph type="secondary">
          {initialized
            ? '登录后继续管理代理服务与出口链路。'
            : '首次使用，请设置用于保护控制台的管理员账户。'}
        </Typography.Paragraph>
        {error && <Alert type="error" showIcon title={error} className="form-alert" />}
        <Form form={form} layout="vertical" onFinish={submit} requiredMark={false}>
          <Form.Item
            name="username"
            label="管理员用户名"
            rules={[
              { required: true, message: '请输入用户名' },
              ...(!initialized ? [{ min: 3, max: 64, message: '用户名长度为 3–64 个字符' }] : []),
            ]}
          >
            <Input size="large" prefix={<UserOutlined />} autoComplete="username" autoFocus />
          </Form.Item>
          <Form.Item
            name="password"
            label="管理员密码"
            rules={[
              { required: true, message: '请输入密码' },
              ...(!initialized ? [passwordRule] : []),
            ]}
          >
            <Input.Password
              size="large"
              prefix={<LockOutlined />}
              autoComplete={initialized ? 'current-password' : 'new-password'}
            />
          </Form.Item>
          {!initialized && (
            <>
              <Progress
                percent={Math.min(
                  100,
                  (password.length >= 12 ? 25 : 0) +
                    (password.length >= 16 ? 25 : 0) +
                    (/[a-z]/.test(password) && /[A-Z]/.test(password) ? 25 : 0) +
                    (/\d/.test(password) && /[^\w]/.test(password) ? 25 : 0),
                )}
                showInfo={false}
                size="small"
              />
              <Form.Item
                name="confirm"
                label="确认密码"
                dependencies={['password']}
                rules={[
                  { required: true, message: '请再次输入密码' },
                  ({ getFieldValue }) => ({
                    validator: (_, value) =>
                      value === getFieldValue('password')
                        ? Promise.resolve()
                        : Promise.reject(new Error('两次输入的密码不一致')),
                  }),
                ]}
              >
                <Input.Password size="large" autoComplete="new-password" />
              </Form.Item>
            </>
          )}
          <Button type="primary" htmlType="submit" size="large" block loading={loading}>
            {initialized ? '登录控制台' : '初始化并进入控制台'}
          </Button>
        </Form>
        <Typography.Paragraph type="secondary" className="auth-note">
          <LockOutlined /> 管理员密码以 bcrypt 哈希保存在本地。
        </Typography.Paragraph>
      </Card>
    </main>
  );
}
