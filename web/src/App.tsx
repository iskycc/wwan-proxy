import { lazy, Suspense, useEffect, useState } from 'react';
import {
  App as AntApp,
  Alert,
  Avatar,
  Badge,
  Breadcrumb,
  Button,
  ConfigProvider,
  Drawer,
  Dropdown,
  Grid,
  Layout,
  Menu,
  Result,
  Select,
  Skeleton,
  Space,
  Spin,
  Typography,
  theme,
} from 'antd';
import zhCN from 'antd/locale/zh_CN';
import {
  ApiOutlined,
  BarChartOutlined,
  DashboardOutlined,
  FileTextOutlined,
  LogoutOutlined,
  MenuFoldOutlined,
  MenuOutlined,
  MenuUnfoldOutlined,
  PlusOutlined,
  ReloadOutlined,
  SafetyOutlined,
  SettingOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { api, errorMessage } from './lib/api';
import { duration, time } from './lib/format';
import { useOverview } from './hooks/useOverview';
import { useSession } from './hooks/useSession';
import AuthPage from './components/AuthPage';
import ServerEditor from './components/ServerEditor';
import type { NetworkInterface, PageKey, ServerConfig, SystemSettings } from './types';

const OverviewPage = lazy(() => import('./pages/OverviewPage'));
const ConnectionsPage = lazy(() => import('./pages/ConnectionsPage'));
const PerformancePage = lazy(() => import('./pages/PerformancePage'));
const LogsPage = lazy(() => import('./pages/LogsPage'));
const EventsPage = lazy(() => import('./pages/EventsPage'));
const StatisticsPage = lazy(() => import('./pages/StatisticsPage'));
const SettingsPage = lazy(() => import('./pages/SettingsPage'));
const pages = [
  {
    key: 'overview',
    label: '网络总览',
    icon: <DashboardOutlined />,
    description: '链路健康、代理会话与实时流量',
  },
  {
    key: 'configuration',
    label: '连接配置',
    icon: <ApiOutlined />,
    description: '管理出口网口、协议和访问策略',
  },
  {
    key: 'performance',
    label: '实时性能',
    icon: <ThunderboltOutlined />,
    description: '实时吞吐趋势与进程运行指标',
  },
  {
    key: 'logs',
    label: '运行日志',
    icon: <FileTextOutlined />,
    description: '搜索运行记录与故障诊断',
  },
  {
    key: 'events',
    label: 'Vohive 事件',
    icon: <SafetyOutlined />,
    description: '设备健康检查与自动恢复记录',
  },
  {
    key: 'statistics',
    label: '历史统计',
    icon: <BarChartOutlined />,
    description: '查看最近七天的流量与连接趋势',
  },
  {
    key: 'settings',
    label: '系统设置',
    icon: <SettingOutlined />,
    description: '系统策略、安全、设备恢复与程序更新',
  },
];
function currentPage(): PageKey {
  const key = location.hash.slice(1);
  return (pages.some((page) => page.key === key) ? key : 'overview') as PageKey;
}
function Dashboard() {
  const session = useSession(),
    { message } = AntApp.useApp();
  const live = useOverview(
    !!session.session?.authenticated,
    session.session?.username || '',
    session.expire,
  );
  const [page, setPage] = useState<PageKey>(currentPage),
    [collapsed, setCollapsed] = useState(false),
    [menuOpen, setMenuOpen] = useState(false);
  const [editor, setEditor] = useState<{
    config?: ServerConfig;
    settings: SystemSettings;
    interfaces: NetworkInterface[];
  } | null>(null);
  const [editing, setEditing] = useState(false),
    [refreshing, setRefreshing] = useState(false);
  const screens = Grid.useBreakpoint(),
    mobile = !screens.lg,
    selected = pages.find((value) => value.key === page)!;
  useEffect(() => {
    const changed = () => setPage(currentPage());
    window.addEventListener('hashchange', changed);
    return () => window.removeEventListener('hashchange', changed);
  }, []);
  useEffect(() => {
    window.scrollTo({ top: 0 });
  }, [page]);
  useEffect(() => {
    if (!session.session?.authenticated) setEditor(null);
  }, [session.session?.authenticated]);
  const navigate = (key: string) => {
    location.hash = key;
    setPage(key as PageKey);
    setMenuOpen(false);
  };
  const refresh = async () => {
    setRefreshing(true);
    try {
      await live.refresh();
    } catch (error) {
      message.error(errorMessage(error));
    } finally {
      setRefreshing(false);
    }
  };
  const changed = () => {
    void live.refresh().catch((error) => message.error(errorMessage(error)));
  };
  const edit = async (config?: ServerConfig) => {
    if (editing) return;
    setEditing(true);
    try {
      // The editor always reads persisted settings, including Vohive enablement.
      const [settings, interfaces] = await Promise.all([
        api<SystemSettings>('/api/settings'),
        api<NetworkInterface[]>('/api/interfaces').catch((error) => {
          message.warning('读取网口失败，可手工输入：' + errorMessage(error));
          return [];
        }),
      ]);
      setEditor({ config: config ? structuredClone(config) : undefined, settings, interfaces });
    } catch (error) {
      message.error('读取系统设置失败：' + errorMessage(error));
    } finally {
      setEditing(false);
    }
  };
  if (!session.session)
    return (
      <div className="boot-screen">
        {session.error ? (
          <Result
            status="error"
            title="无法连接控制服务"
            subTitle={session.error}
            extra={<Button onClick={() => void session.refresh().catch(() => {})}>重新连接</Button>}
          />
        ) : (
          <Space orientation="vertical" align="center" size={20}>
            <Spin size="large" />
            <Typography.Text type="secondary">正在验证登录会话…</Typography.Text>
          </Space>
        )}
      </div>
    );
  if (!session.session.authenticated)
    return (
      <AuthPage initialized={session.session.initialized} onAuthenticated={session.setSession} />
    );
  const navigation = (
    <Menu
      mode="inline"
      selectedKeys={[page]}
      items={pages.map(({ key, label, icon }) => ({ key, label, icon }))}
      onClick={({ key }) => navigate(key)}
    />
  );
  const overview = live.overview;
  const username = session.session.username || '';
  return (
    <Layout className="app-layout">
      {!mobile && (
        <Layout.Sider className="app-sider" theme="light" width={232} collapsed={collapsed}>
          <div className="sidebar-brand">
            <div className="brand-mark small">W</div>
            {!collapsed && (
              <div>
                <Typography.Text strong>WWAN Control</Typography.Text>
                <small>网络控制中心</small>
              </div>
            )}
          </div>
          {navigation}
          {!collapsed && (
            <div className="sider-status">
              <Badge
                status={live.connection === 'connected' ? 'success' : 'processing'}
                text={live.connection === 'connected' ? '实时服务在线' : '正在连接'}
              />
              <small>
                {overview ? '已运行 ' + duration(overview.uptime_seconds) : '等待同步状态'}
              </small>
            </div>
          )}
        </Layout.Sider>
      )}
      <Drawer
        title="WWAN Control"
        placement="left"
        open={mobile && menuOpen}
        size="min(300px, calc(100vw - 32px))"
        onClose={() => setMenuOpen(false)}
        styles={{ body: { padding: 0 } }}
      >
        {navigation}
      </Drawer>
      <Layout className="main-layout">
        <Layout.Header className="app-header">
          <Space className="header-navigation">
            <Button
              type="text"
              aria-label={mobile ? '打开导航菜单' : '折叠导航菜单'}
              icon={
                mobile ? (
                  <MenuOutlined />
                ) : collapsed ? (
                  <MenuUnfoldOutlined />
                ) : (
                  <MenuFoldOutlined />
                )
              }
              onClick={() => (mobile ? setMenuOpen(true) : setCollapsed(!collapsed))}
            />
            <Breadcrumb
              items={
                mobile
                  ? [{ title: selected.label }]
                  : [{ title: '控制中心' }, { title: selected.label }]
              }
            />
          </Space>
          <Space className="header-actions">
            {!mobile && (
              <Badge
                className="connection-badge"
                status={live.connection === 'connected' ? 'success' : 'warning'}
                text={live.connection === 'connected' ? '实时在线' : '重新连接中'}
              />
            )}
            <ThemeSelect />
            <div className="account-menu">
              <Dropdown
                trigger={['click']}
                placement="bottomRight"
                getPopupContainer={(trigger) => trigger.parentElement!}
                menu={{
                  items: [
                    {
                      key: 'username',
                      label: <span className="break-text">{username}</span>,
                      disabled: true,
                    },
                    { type: 'divider' },
                    { key: 'logout', label: '退出登录', icon: <LogoutOutlined /> },
                  ],
                  onClick: ({ key }) => {
                    if (key === 'logout')
                      void session.logout().catch((error) => message.error(errorMessage(error)));
                  },
                }}
              >
                <Button
                  type="text"
                  className="account-button"
                  aria-label="账户菜单"
                  title={username}
                >
                  <Avatar size="small" style={{ background: '#1677ff' }}>
                    {Array.from(username)[0]?.toUpperCase()}
                  </Avatar>
                  <span className="account-name">{username}</span>
                </Button>
              </Dropdown>
            </div>
          </Space>
        </Layout.Header>
        <Layout.Content className="app-content">
          <div className="page-heading">
            <div>
              <Typography.Title level={2}>{selected.label}</Typography.Title>
              <Typography.Text type="secondary">{selected.description}</Typography.Text>
            </div>
            <Space wrap>
              {['overview', 'configuration', 'performance'].includes(page) && (
                <Button
                  aria-label="刷新实时状态"
                  icon={<ReloadOutlined />}
                  loading={refreshing}
                  onClick={refresh}
                >
                  刷新
                </Button>
              )}
              {['overview', 'configuration'].includes(page) && (
                <Button
                  type="primary"
                  icon={<PlusOutlined />}
                  loading={editing}
                  onClick={() => edit()}
                >
                  新建出口
                </Button>
              )}
            </Space>
          </div>
          {live.connection === 'disconnected' && (
            <Alert
              showIcon
              type="warning"
              title="实时连接已断开，正在自动重连。当前显示最近一次同步的数据。"
              className="form-alert"
            />
          )}
          <Suspense fallback={<CardSkeleton />}>
            {page === 'logs' ? (
              <LogsPage />
            ) : page === 'events' ? (
              <EventsPage />
            ) : page === 'settings' ? (
              <SettingsPage
                servers={overview?.servers || []}
                username={username}
                refreshSession={session.refresh}
                onExpired={session.expire}
              />
            ) : !overview ? (
              <CardSkeleton />
            ) : page === 'configuration' ? (
              <ConnectionsPage overview={overview} onEdit={edit} onChanged={changed} />
            ) : page === 'performance' ? (
              <PerformancePage overview={overview} history={live.history} />
            ) : page === 'statistics' ? (
              <StatisticsPage servers={overview.servers} />
            ) : (
              <OverviewPage overview={overview} onEdit={edit} />
            )}
          </Suspense>
          <footer className="app-footer">
            <Typography.Text type="secondary">
              WWAN Control · {overview ? '最近同步 ' + time(overview.sampled_at) : '正在同步'}
            </Typography.Text>
          </footer>
        </Layout.Content>
      </Layout>
      {editor && <ServerEditor {...editor} onClose={() => setEditor(null)} onSaved={changed} />}
    </Layout>
  );
}
function CardSkeleton() {
  return (
    <div className="page-skeleton">
      <Skeleton active paragraph={{ rows: 8 }} />
    </div>
  );
}
function ThemeSelect() {
  return (
    <Select
      aria-label="界面主题"
      value={document.documentElement.dataset.preference || 'system'}
      style={{ width: 100 }}
      onChange={(value) => window.dispatchEvent(new CustomEvent('theme-change', { detail: value }))}
      options={[
        { value: 'system', label: '跟随系统' },
        { value: 'light', label: '浅色主题' },
        { value: 'dark', label: '深色主题' },
      ]}
    />
  );
}
export default function App() {
  const [preference, setPreference] = useState(() => {
    try {
      return localStorage.getItem('wwan-control.theme') || 'system';
    } catch {
      return 'system';
    }
  });
  const [systemDark, setSystemDark] = useState(
    () => matchMedia('(prefers-color-scheme: dark)').matches,
  );
  const dark = preference === 'dark' || (preference === 'system' && systemDark);
  useEffect(() => {
    const media = matchMedia('(prefers-color-scheme: dark)'),
      systemChange = () => setSystemDark(media.matches);
    const change = (event: Event) => {
      const value = (event as CustomEvent<string>).detail;
      setPreference(value);
      try {
        localStorage.setItem('wwan-control.theme', value);
      } catch {}
    };
    media.addEventListener('change', systemChange);
    window.addEventListener('theme-change', change);
    return () => {
      media.removeEventListener('change', systemChange);
      window.removeEventListener('theme-change', change);
    };
  }, []);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  document.documentElement.dataset.preference = preference;
  return (
    <ConfigProvider
      locale={zhCN}
      theme={{
        algorithm: dark ? theme.darkAlgorithm : theme.defaultAlgorithm,
        token: {
          colorPrimary: '#1677ff',
          borderRadius: 10,
          fontFamily:
            '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif',
          colorBgLayout: dark ? '#101419' : '#f4f6fa',
        },
        components: {
          Layout: { headerBg: dark ? '#141414' : '#ffffff', siderBg: dark ? '#141414' : '#ffffff' },
          Table: { cellPaddingBlock: 14 },
          Menu: { itemHeight: 46 },
        },
      }}
    >
      <AntApp>
        <Dashboard />
      </AntApp>
    </ConfigProvider>
  );
}
