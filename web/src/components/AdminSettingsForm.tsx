import { useState } from 'react';
import { Alert, App, Button, Card, Form, Input, Typography } from 'antd';
import { LockOutlined } from '@ant-design/icons';
import { api, errorMessage, jsonBody } from '../lib/api';

export default function AdminSettingsForm({
  username,
  onSaved,
}: {
  username: string;
  onSaved: () => Promise<unknown>;
}) {
  const [form] = Form.useForm(),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const { message } = App.useApp();
  const save = async (value: {
    username: string;
    current_password: string;
    new_password?: string;
  }) => {
    setBusy(true);
    setError('');
    try {
      await api('/api/admin', {
        method: 'PUT',
        ...jsonBody({
          username: value.username.trim(),
          current_password: value.current_password,
          new_password: value.new_password || '',
        }),
      });
      form.resetFields(['current_password', 'new_password', 'confirm']);
      await onSaved();
      message.success('管理员信息已更新，其他登录会话已撤销');
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card title="管理员安全">
      <Typography.Paragraph type="secondary">
        修改账户需要验证当前密码。新密码留空时保留原密码。
      </Typography.Paragraph>
      {error && <Alert className="form-alert" type="error" showIcon title={error} />}
      <Form
        form={form}
        layout="vertical"
        initialValues={{ username }}
        onFinish={save}
        disabled={busy}
      >
        <Form.Item
          name="username"
          label="管理员用户名"
          rules={[{ required: true }, { min: 3, max: 64, message: '用户名长度为 3–64 个字符' }]}
        >
          <Input autoComplete="username" />
        </Form.Item>
        <Form.Item
          name="current_password"
          label="当前密码"
          rules={[{ required: true, message: '请输入当前密码' }]}
        >
          <Input.Password autoComplete="current-password" />
        </Form.Item>
        <Form.Item
          name="new_password"
          label="新密码"
          rules={[
            {
              validator: (_, value) => {
                if (!value) return Promise.resolve();
                const length = new TextEncoder().encode(value).length;
                return length >= 12 && length <= 72
                  ? Promise.resolve()
                  : Promise.reject(new Error('密码长度必须为 12–72 字节'));
              },
            },
          ]}
        >
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        <Form.Item
          name="confirm"
          label="确认新密码"
          dependencies={['new_password']}
          rules={[
            ({ getFieldValue }) => ({
              validator: (_, value) =>
                (value || '') === (getFieldValue('new_password') || '')
                  ? Promise.resolve()
                  : Promise.reject(new Error('两次输入的新密码不一致')),
            }),
          ]}
        >
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        <Button htmlType="submit" icon={<LockOutlined />} loading={busy}>
          更新管理员信息
        </Button>
      </Form>
    </Card>
  );
}
