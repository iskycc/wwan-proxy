import { useEffect, useRef, useState } from 'react';
import {
  Alert,
  App,
  AutoComplete,
  Button,
  Col,
  Form,
  Grid,
  Input,
  InputNumber,
  Modal,
  Radio,
  Row,
  Select,
  Switch,
  Tabs,
  Typography,
} from 'antd';
import { DeleteOutlined, PlusOutlined, SaveOutlined } from '@ant-design/icons';
import { api, errorMessage, jsonBody } from '../lib/api';
import { formToServer, parsePorts, serverToForm, type ServerFormValues } from '../lib/serverForm';
import type { NetworkInterface, ServerConfig, SystemSettings } from '../types';
import KeyValueFields from './KeyValueFields';

const required = [{ required: true, message: '请填写此项' }];
const field = (
  name: string | string[],
  label: string,
  placeholder?: string,
  requiredField = true,
) => (
  <Form.Item name={name} label={label} rules={requiredField ? required : undefined}>
    <Input placeholder={placeholder} />
  </Form.Item>
);
const number = (name: string | string[], label: string, max?: number, min = 0) => (
  <Form.Item name={name} label={label} rules={required}>
    <InputNumber min={min} max={max} style={{ width: '100%' }} />
  </Form.Item>
);
const toggle = (name: string | string[], label: string) => (
  <Form.Item name={name} label={label} valuePropName="checked">
    <Switch />
  </Form.Item>
);

export default function ServerEditor({
  config,
  settings,
  interfaces,
  onClose,
  onSaved,
}: {
  config?: ServerConfig;
  settings: SystemSettings;
  interfaces: NetworkInterface[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [form] = Form.useForm<ServerFormValues>();
  const [saving, setSaving] = useState(false),
    [error, setError] = useState('');
  const { message, modal } = App.useApp();
  const auth = Form.useWatch(['auth', 'method'], form);
  const http = Form.useWatch(['http_proxy', 'enabled'], form);
  const upstream = Form.useWatch(['upstream', 'enabled'], form);
  const upstreamAuth = Form.useWatch(['upstream', 'auth_method'], form);
  const bind = Form.useWatch(['bind', 'enabled'], form);
  const dns = Form.useWatch('dns_mode', form);
  const udp = Form.useWatch(['udp', 'enabled'], form);
  const ports = Form.useWatch('udp_ports', form);
  const listen = Form.useWatch('listen', form);
  const admission = Form.useWatch('admission_cidrs', form);
  const [tab, setTab] = useState('basic');
  const compact = !Grid.useBreakpoint().md;
  const formContainer = useRef<HTMLDivElement>(null);
  const [invalidField, setInvalidField] = useState<Parameters<typeof form.scrollToField>[0] | null>(
    null,
  );
  useEffect(() => {
    formContainer.current?.parentElement?.scrollTo({ top: 0 });
  }, [tab]);
  useEffect(() => {
    if (invalidField) {
      // Tabs reveal the new pane in a subsequent render before it can receive focus.
      const frame = requestAnimationFrame(() => {
        form.scrollToField(invalidField, { focus: true, block: 'center' });
        setInvalidField(null);
      });
      return () => cancelAnimationFrame(frame);
    }
  }, [invalidField, tab, form]);
  const changeTab = (key: string) => {
    setTab(key);
  };
  const onSave = async () => {
    setSaving(true);
    setError('');
    try {
      const data = formToServer(form.getFieldsValue(true));
      await api(config?.id ? '/api/servers/' + config.id : '/api/servers', {
        method: config?.id ? 'PUT' : 'POST',
        ...jsonBody(data),
      });
      message.success('出口配置已保存并应用');
      onSaved();
      onClose();
    } catch (error) {
      setError(errorMessage(error));
      formContainer.current?.parentElement?.scrollTo({ top: 0 });
    } finally {
      setSaving(false);
    }
  };
  const remove = async () =>
    await modal.confirm({
      title: '删除出口配置',
      content: '将删除此出口及其心跳和历史统计，正在处理的代理连接会关闭。',
      okText: '删除配置',
      okButtonProps: { danger: true, 'aria-label': '删除配置' },
      cancelText: '取消',
      onOk: async () => {
        try {
          await api('/api/servers/' + config!.id, { method: 'DELETE' });
          message.success('出口配置已删除');
          onSaved();
          onClose();
        } catch (error) {
          message.error(errorMessage(error));
          throw error;
        }
      },
    });
  const basic = (
    <>
      <Row gutter={20}>
        <Col xs={24} md={12}>
          {field('name', '出口名称', '例如：移动网络 01')}
        </Col>
        <Col xs={24} md={12}>
          <Form.Item
            name="interface"
            label="出口网口"
            rules={required}
            extra="支持物理或虚拟 Linux 网口，也可以手工输入。"
          >
            <AutoComplete
              options={interfaces.map((item) => ({
                value: item.name,
                label: item.name + ' · ' + item.addresses.join(', '),
              }))}
              placeholder="例如 eth0、wwan0"
            />
          </Form.Item>
        </Col>
      </Row>
      {settings.vohive.enabled && field('vohive_device_id', 'Vohive 设备编码', '例如 Y2', false)}
      <Row gutter={20}>
        <Col xs={24} md={12}>
          {field('listen', 'SOCKS5 监听地址', '0.0.0.0:1080')}
        </Col>
        <Col xs={24} md={12}>
          {number('max_connections', '实例最大连接数（0 表示不限）')}
        </Col>
      </Row>
      <Row gutter={20}>
        <Col xs={24} md={12}>
          {field('connect_timeout', '连接超时', '10s')}
        </Col>
        <Col xs={24} md={12}>
          {field('idle_timeout', 'TCP 空闲超时', '5m')}
        </Col>
      </Row>
      {toggle('enabled', '保存后启用此实例')}
      {auth === 'none' &&
        !admission?.trim() &&
        !/^(127\.|\[::1\]|localhost:)/.test(listen || '') && (
          <Alert
            type="warning"
            showIcon
            title="该监听器允许来源不受限的匿名访问，请配置认证或来源网段限制。"
          />
        )}
    </>
  );
  const protocols = (
    <>
      <Form.Item name={['auth', 'method']} label="代理认证方式">
        <Select
          options={[
            { value: 'none', label: '无需认证' },
            { value: 'username_password', label: '用户名与密码' },
          ]}
        />
      </Form.Item>
      {auth === 'username_password' && (
        <div className="list-fields">
          <Typography.Paragraph type="secondary">
            已有用户留空密码可保留原密码；新增或重命名用户需要设置密码。
          </Typography.Paragraph>
          <Form.List
            name="auth_users"
            rules={[
              {
                validator: (_, rows) =>
                  rows?.length
                    ? Promise.resolve()
                    : Promise.reject(new Error('至少添加一个认证用户')),
              },
            ]}
          >
            {(fields, { add, remove }, { errors }) => (
              <>
                {fields.map((row) => (
                  <div key={row.key} className="dynamic-row">
                    <Form.Item name={[row.name, 'username']} label="用户名" rules={required}>
                      <Input aria-label="代理用户名" placeholder="用户名" autoComplete="off" />
                    </Form.Item>
                    <Form.Item name={[row.name, 'password']} label="密码">
                      <Input.Password
                        aria-label="代理用户密码"
                        placeholder="留空保留原密码"
                        autoComplete="new-password"
                      />
                    </Form.Item>
                    <Button
                      type="text"
                      danger
                      aria-label="删除认证用户"
                      icon={<DeleteOutlined />}
                      onClick={() => remove(row.name)}
                    />
                  </div>
                ))}
                <Form.ErrorList errors={errors} />
                <Button
                  type="dashed"
                  icon={<PlusOutlined />}
                  onClick={() => add({ username: '', password: '', unchanged: false })}
                >
                  添加认证用户
                </Button>
              </>
            )}
          </Form.List>
        </div>
      )}
      <Typography.Title level={5}>HTTP / HTTPS 代理</Typography.Title>
      {toggle(['http_proxy', 'enabled'], '启用 HTTP / HTTPS 代理')}
      {http && field(['http_proxy', 'listen'], 'HTTP 监听地址', '0.0.0.0:8080')}
      <Typography.Title level={5}>链式 SOCKS5 上游</Typography.Title>
      {toggle(['upstream', 'enabled'], '启用上游代理')}
      {upstream && (
        <>
          <Alert
            type="info"
            showIcon
            title="TCP 和原生 UDP 经过上游代理；链式 BIND 暂不支持。"
            className="form-alert"
          />
          {field(['upstream', 'address'], '上游地址', 'proxy.example.com:1080')}
          <Form.Item name={['upstream', 'auth_method']} label="上游认证">
            <Select
              options={[
                { value: 'none', label: '无需认证' },
                { value: 'username_password', label: '用户名与密码' },
              ]}
            />
          </Form.Item>
          {upstreamAuth === 'username_password' && (
            <Row gutter={20}>
              <Col xs={24} md={12}>
                {field(['upstream', 'username'], '上游用户名')}
              </Col>
              <Col xs={24} md={12}>
                <Form.Item
                  name={['upstream', 'password']}
                  label="上游密码"
                  extra={config ? '留空保留已有密码' : undefined}
                  rules={config ? undefined : required}
                >
                  <Input.Password autoComplete="new-password" />
                </Form.Item>
              </Col>
            </Row>
          )}
        </>
      )}
      <Typography.Title level={5}>SOCKS5 BIND</Typography.Title>
      {toggle(['bind', 'enabled'], '允许 BIND 命令')}
      {bind && (
        <Row gutter={20}>
          <Col xs={24} md={12}>
            {field(['bind', 'advertise'], 'BIND 广播地址', 'auto')}
          </Col>
          <Col xs={24} md={12}>
            {field('bind_timeout', 'BIND 等待超时', '2m')}
          </Col>
        </Row>
      )}
    </>
  );
  const access = (
    <>
      <Form.Item
        name="admission_cidrs"
        label="允许访问的来源网段"
        extra="逗号、空格或换行分隔；留空允许所有来源。"
      >
        <Input.TextArea rows={3} placeholder={'192.168.0.0/16\n10.0.0.0/8'} />
      </Form.Item>
      <Form.Item name={['access', 'target_default']} label="目标默认动作">
        <Select
          options={[
            { value: 'allow', label: '允许' },
            { value: 'deny', label: '拒绝' },
          ]}
        />
      </Form.Item>
      <Form.Item
        name="target_rules"
        label="目标 ACL"
        extra="按顺序匹配，第一条命中生效。支持域名、IP、CIDR 和端口范围。"
      >
        <Input.TextArea
          rows={5}
          placeholder={'deny *.example.com:443\ndeny 192.0.2.0/24:1-1023\nallow *'}
        />
      </Form.Item>
      <Row gutter={20}>
        <Col xs={24} md={12}>
          {number(['access', 'max_connections_per_ip'], '每来源 IP 最大连接数')}
        </Col>
        <Col xs={24} md={12}>
          {number(['access', 'max_udp_associations_per_ip'], '每来源 IP 最大 UDP 会话数')}
        </Col>
      </Row>
      <Typography.Text type="secondary">
        并发限制为 0 时不单独限制；TCP、HTTP 和热重载前后的会话共用额度。
      </Typography.Text>
    </>
  );
  const dnsFields = (
    <>
      <Form.Item name="dns_mode" label="DNS 模式">
        <Select
          options={[
            { value: 'system', label: '系统 DNS' },
            { value: 'servers', label: '指定传统 DNS' },
            { value: 'doh', label: 'DNS over HTTPS' },
          ]}
        />
      </Form.Item>
      {toggle(['dns', 'ipv4_only'], '域名仅解析 IPv4（不请求 AAAA）')}
      {dns === 'system' && (
        <Alert
          type="info"
          showIcon
          title="系统 DNS 也绑定出口网口。若系统使用回环 DNS stub，请选择可经出口访问的 DNS 或 DoH。"
        />
      )}
      {dns === 'servers' && (
        <Form.Item
          name="dns_servers"
          label="DNS 服务器"
          rules={required}
          extra="可填写多个地址，默认端口为 53。"
        >
          <Input.TextArea rows={3} placeholder={'114.114.114.114:53\n8.8.8.8:53'} />
        </Form.Item>
      )}
      {dns === 'doh' && (
        <>
          <Form.Item
            name="doh_urls"
            label="DoH 端点"
            rules={required}
            extra="每行一个 HTTPS URL；多个端点并发查询，采用最快的有效结果。"
          >
            <Input.TextArea rows={3} placeholder="https://dns.google/dns-query" />
          </Form.Item>
          <Form.Item
            name="doh_bootstrap"
            label="Bootstrap DNS 服务器"
            extra="用于解析 DoH 域名的传统 DNS 服务器，例如 114.114.114.114:53。"
          >
            <Input placeholder="114.114.114.114:53" />
          </Form.Item>
          {field(['dns', 'doh', 'timeout'], 'DoH 超时', '10s')}
          {toggle(['dns', 'doh', 'insecure_skip_verify'], '跳过 DoH TLS 证书校验')}
          <KeyValueFields
            name="doh_headers"
            title="DoH 请求头"
            labels={['请求头名称', '请求头值']}
          />
        </>
      )}
    </>
  );
  const udpFields = (
    <>
      {toggle(['udp', 'enabled'], '启用 UDP ASSOCIATE')}
      {udp && (
        <>
          <Row gutter={20}>
            <Col xs={24} md={12}>
              {number(['udp', 'max_associations'], '实例最大 UDP 会话数', undefined, 1)}
            </Col>
            <Col xs={24} md={12}>
              {field(['udp', 'idle_timeout'], 'UDP 空闲超时', '2m')}
            </Col>
          </Row>
          {toggle(['udp', 'strict_endpoint'], '严格校验回包目标 IP 与端口')}
          <Row gutter={20}>
            <Col xs={24} md={12}>
              {field(['udp', 'bind_ip'], 'UDP 监听 IP', '0.0.0.0')}
            </Col>
            <Col xs={24} md={12}>
              {field(['udp', 'advertise'], 'UDP 广播 IP', 'auto')}
            </Col>
          </Row>
          <Form.Item
            name="udp_ports"
            label="固定 Relay 端口池"
            extra="逗号或空格分隔，最多 4096 个端口；留空使用随机范围。"
            rules={[
              {
                validator: (_, value) => {
                  try {
                    parsePorts(value || '');
                    return Promise.resolve();
                  } catch (error) {
                    return Promise.reject(error);
                  }
                },
              },
            ]}
          >
            <Input placeholder="12000, 12007, 53000" />
          </Form.Item>
          {!ports?.trim() && (
            <Row gutter={20}>
              <Col xs={24} md={12}>
                {number(['udp', 'port_min'], '随机端口下限', 65535, 1024)}
              </Col>
              <Col xs={24} md={12}>
                {number(['udp', 'port_max'], '随机端口上限', 65535, 1024)}
              </Col>
            </Row>
          )}
          <KeyValueFields
            name="udp_map"
            title="监听地址映射"
            labels={['本地监听 IP', '对外广播 IP']}
            help="NAT 环境下，将本地监听地址映射到公网 Relay 地址。"
          />
          <KeyValueFields
            name="udp_source_map"
            title="客户端来源映射"
            labels={['客户端 IP 或 CIDR', '来源对应广播 IP']}
            help="根据客户端来源 IP 或网段选择返回的 UDP Relay 地址。"
          />
        </>
      )}
    </>
  );
  const heartbeat = (
    <>
      <Alert
        type="info"
        showIcon
        title="健康检查与代理流量共用出口网口，结果会显示在总览和历史统计中。"
        className="form-alert"
      />
      {field(['heartbeat', 'url'], '心跳 URL', 'https://1.1.1.1/cdn-cgi/trace')}
      <Row gutter={20}>
        <Col xs={24} md={12}>
          {field(['heartbeat', 'interval'], '心跳间隔', '30s')}
        </Col>
        <Col xs={24} md={12}>
          {field(['heartbeat', 'timeout'], '心跳超时', '12s')}
        </Col>
      </Row>
    </>
  );
  return (
    <Modal
      className="server-editor"
      open
      centered
      title={
        <Typography.Text strong ellipsis={{ tooltip: config?.name }}>
          {config ? '编辑出口 · ' + config.name : '新建出口'}
        </Typography.Text>
      }
      width={960}
      styles={{ body: { maxHeight: 'min(68dvh, calc(100dvh - 210px))', overflowY: 'auto' } }}
      onCancel={() => !saving && onClose()}
      mask={{ closable: false }}
      footer={
        <div className="editor-footer">
          {config && (
            <Button
              danger
              aria-label="删除配置"
              icon={<DeleteOutlined />}
              disabled={saving}
              onClick={remove}
            >
              删除配置
            </Button>
          )}
          <span />
          <Button aria-label="取消" onClick={onClose} disabled={saving}>
            取消
          </Button>
          <Button
            type="primary"
            icon={<SaveOutlined />}
            loading={saving}
            onClick={() => form.submit()}
          >
            保存并应用
          </Button>
        </div>
      }
    >
      <div ref={formContainer}>
        {error && <Alert type="error" showIcon title={error} className="form-alert" />}
        <Form
          form={form}
          disabled={saving}
          layout="vertical"
          initialValues={serverToForm(config)}
          onFinish={onSave}
          onFinishFailed={({ errorFields }) => {
            const name = errorFields[0]?.name[0];
            if (errorFields[0]) setInvalidField(errorFields[0].name);
            setTab(
              name === 'auth_users' ||
                name === 'auth' ||
                name === 'http_proxy' ||
                name === 'upstream' ||
                name === 'bind' ||
                name === 'bind_timeout'
                ? 'protocols'
                : name === 'access' || name === 'admission_cidrs' || name === 'target_rules'
                  ? 'access'
                  : name === 'dns' ||
                      name === 'doh_urls' ||
                      name === 'dns_servers' ||
                      name === 'doh_headers'
                    ? 'dns'
                    : name === 'udp' || String(name).startsWith('udp_')
                      ? 'udp'
                      : name === 'heartbeat'
                        ? 'heartbeat'
                        : 'basic',
            );
          }}
        >
          {compact && (
            <Form.Item label="配置分组">
              <Radio.Group
                className="editor-section-selector"
                aria-label="配置分组"
                value={tab}
                onChange={(event) => changeTab(event.target.value)}
                optionType="button"
                buttonStyle="solid"
                options={[
                  { value: 'basic', label: '基本设置' },
                  { value: 'protocols', label: '协议与认证' },
                  { value: 'access', label: '访问控制' },
                  { value: 'dns', label: 'DNS 解析' },
                  { value: 'udp', label: 'UDP 中继' },
                  { value: 'heartbeat', label: '出口心跳' },
                ]}
              />
            </Form.Item>
          )}
          <Tabs
            activeKey={tab}
            onChange={changeTab}
            tabBarStyle={compact ? { display: 'none' } : undefined}
            items={[
              { key: 'basic', label: '基本设置', children: basic, forceRender: true },
              { key: 'protocols', label: '协议与认证', children: protocols, forceRender: true },
              { key: 'access', label: '访问控制', children: access, forceRender: true },
              { key: 'dns', label: 'DNS 解析', children: dnsFields, forceRender: true },
              { key: 'udp', label: 'UDP 中继', children: udpFields, forceRender: true },
              { key: 'heartbeat', label: '出口心跳', children: heartbeat, forceRender: true },
            ]}
          />
        </Form>
      </div>
    </Modal>
  );
}
