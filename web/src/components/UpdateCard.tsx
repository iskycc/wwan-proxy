import { useEffect, useState } from 'react';
import {
  Alert,
  App,
  Button,
  Card,
  Descriptions,
  Progress,
  Select,
  Space,
  Tag,
  Typography,
} from 'antd';
import { CloudDownloadOutlined, SyncOutlined } from '@ant-design/icons';
import { api, errorMessage, jsonBody } from '../lib/api';
import { useQuery } from '../hooks/useQuery';
import { time } from '../lib/format';
import type { ServerConfig, UpdateInfo } from '../types';

export default function UpdateCard({ servers }: { servers: ServerConfig[] }) {
  const initial = useQuery<UpdateInfo>('/api/update');
  const [info, setInfo] = useState<UpdateInfo | null>(null),
    [route, setRoute] = useState(''),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(''),
    [restarting, setRestarting] = useState(false);
  const { message, modal } = App.useApp();
  const running = ['queued', 'running'].includes(info?.operation?.state || '');
  const interfaces = [...new Set(servers.map((server) => server.interface))].sort();
  useEffect(() => {
    if (initial.data) setInfo(initial.data);
  }, [initial.data]);
  useEffect(() => {
    if (route && !interfaces.includes(route)) setRoute('');
  }, [route, interfaces.join('\n')]);
  useEffect(() => {
    if (!running) return;
    let stopped = false,
      timer: ReturnType<typeof setTimeout>;
    const deadline = Date.now() + 5 * 60 * 1000;
    const poll = async () => {
      try {
        const value = await api<UpdateInfo>('/api/update');
        if (stopped) return;
        setInfo(value);
        setRestarting(false);
        if (value.operation?.state === 'succeeded') {
          message.success('程序更新完成');
          return;
        }
        if (value.operation?.state === 'failed') {
          message.error('程序更新失败，请查看更新详情');
          return;
        }
      } catch {
        if (!stopped) setRestarting(true);
      }
      if (stopped) return;
      if (Date.now() >= deadline) {
        setError('等待更新超时，请刷新查看更新状态');
        return;
      }
      timer = setTimeout(poll, 2000);
    };
    timer = setTimeout(poll, 2000);
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [running, message]);
  const check = async () => {
    setBusy(true);
    setError('');
    try {
      const value = await api<UpdateInfo>(
        '/api/update?' + new URLSearchParams({ refresh: '1', interface: route }),
      );
      setInfo(value);
      message.success(
        value.update_available ? '发现新版本 ' + value.latest?.version : '当前已经是最新版本',
      );
    } catch (error) {
      setError(errorMessage(error));
    } finally {
      setBusy(false);
    }
  };
  const install = async () =>
    await modal.confirm({
      title: '安装程序更新',
      content:
        '将更新到 ' +
        info?.latest?.version +
        '。下载使用' +
        (route ? '网口 ' + route : '系统默认路由') +
        '，服务会短暂重启并中断现有代理连接。',
      okText: '开始更新',
      okButtonProps: { 'aria-label': '开始更新' },
      onOk: async () => {
        try {
          await api('/api/update', { method: 'POST', ...jsonBody({ interface: route }) });
          setInfo((value) =>
            value
              ? {
                  ...value,
                  operation: {
                    state: 'queued',
                    started_at: new Date().toISOString(),
                    interface: route,
                    message: '更新任务已排队',
                  },
                }
              : value,
          );
        } catch (error) {
          message.error(errorMessage(error));
          throw error;
        }
      },
    });
  const operation = info?.operation;
  return (
    <Card
      className="update-card"
      title="程序更新"
      extra={
        <Tag color={running ? 'processing' : info?.update_available ? 'orange' : 'default'}>
          {running
            ? '正在更新'
            : info?.checked
              ? info.update_available
                ? '发现新版本'
                : '已是最新'
              : '尚未检查'}
        </Tag>
      }
    >
      {(error || initial.error) && (
        <Alert type="error" title={error || initial.error} showIcon className="form-alert" />
      )}
      <Descriptions
        column={1}
        size="small"
        items={[
          {
            key: 'version',
            label: '当前版本',
            children: (
              <Typography.Text className="version-text">
                {info?.current_version || '—'}
              </Typography.Text>
            ),
          },
          {
            key: 'platform',
            label: '运行平台',
            children: info ? info.platform + ' · ' + info.architecture : '—',
          },
          {
            key: 'latest',
            label: '最新版本',
            children: info?.latest ? (
              <Typography.Text className="version-text">{info.latest.version}</Typography.Text>
            ) : (
              '尚未检查'
            ),
          },
          ...(info?.latest
            ? [{ key: 'published', label: '发布时间', children: time(info.latest.published_at) }]
            : []),
        ]}
      />
      <Typography.Paragraph type="secondary" className="update-agent-status">
        {info?.install_supported
          ? '更新代理运行正常'
          : info?.install_message || '安装更新前需要本机更新代理。'}
      </Typography.Paragraph>
      <Space orientation="vertical" className="full-width" size={12}>
        <Select
          className="full-width"
          aria-label="更新下载网口"
          value={route}
          onChange={setRoute}
          disabled={running}
          options={[
            { value: '', label: '系统默认路由' },
            ...interfaces.map((value) => ({ value, label: value })),
          ]}
        />
        <Space wrap>
          <Button
            aria-label="检查更新"
            icon={<SyncOutlined />}
            loading={busy || initial.loading}
            disabled={running}
            onClick={check}
          >
            检查更新
          </Button>
          <Button
            type="primary"
            aria-label="安装更新"
            icon={<CloudDownloadOutlined />}
            disabled={
              !info?.checked || !info?.update_available || !info?.install_supported || running
            }
            onClick={install}
          >
            安装更新
          </Button>
        </Space>
        {operation && (
          <>
            {running && <Progress percent={100} status="active" showInfo={false} />}
            <Alert
              showIcon
              type={
                operation.state === 'failed'
                  ? 'error'
                  : operation.state === 'succeeded'
                    ? 'success'
                    : 'info'
              }
              title={
                restarting
                  ? '服务正在重启，正在等待重新连接…'
                  : operation.state === 'queued'
                    ? '更新任务已排队'
                    : operation.state === 'running'
                      ? '正在下载安装'
                      : operation.state === 'failed'
                        ? '更新失败'
                        : '更新完成'
              }
              description={
                <span className="break-text">
                  {operation.message || ''}
                  {operation.interface && (
                    <>
                      <br />
                      下载网口：{operation.interface}
                    </>
                  )}
                </span>
              }
            />
          </>
        )}
      </Space>
    </Card>
  );
}
