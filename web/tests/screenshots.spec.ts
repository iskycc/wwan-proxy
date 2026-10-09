import { test, expect } from './fixtures';
import path from 'node:path';

test('生成 Ant Design 页面预览', async ({ page, backend }) => {
  test.setTimeout(60000);
  await page.setViewportSize({ width: 1440, height: 1000 });
  const docs = path.resolve(import.meta.dirname, '../../docs');
  backend.auth = { initialized: false, authenticated: false };
  await page.goto('/');
  await expect(page.getByRole('button', { name: '初始化并进入控制台' })).toBeVisible();
  await page.screenshot({
    path: path.join(docs, 'webui-initialization.png'),
    fullPage: true,
    animations: 'disabled',
  });
  backend.auth = { initialized: true, authenticated: true, username: 'administrator' };
  const server = backend.overview.servers[0];
  server.enabled = true;
  backend.overview.servers.push({
    ...structuredClone(server),
    id: 2,
    name: '联通网络 02',
    interface: 'wwan1',
    listen: '0.0.0.0:1081',
    vohive_device_id: 'Y3',
  });
  server.interface = 'wwan0';
  backend.overview.instances = backend.overview.servers.map((server) => ({
    id: server.id!,
    name: server.name,
    enabled: true,
    running: true,
    http_running: false,
    listen: server.listen,
    interface: server.interface,
    started_at: new Date(Date.now() - 3600000).toISOString(),
    metrics: {
      active_connections: 12,
      active_udp: 2,
      total_connections: 12560,
      tcp_upload_bytes: 655360000,
      tcp_download_bytes: 2147483648,
      udp_upload_bytes: 20971520,
      udp_download_bytes: 52428800,
    },
    http_metrics: {},
  }));
  backend.overview.heartbeats = Object.fromEntries(
    backend.overview.servers.map((server, index) => [
      server.id,
      {
        checked_at: new Date().toISOString(),
        healthy: true,
        latency_ms: 56 + index * 12,
        status_code: 200,
        public_ip: '203.0.113.' + (21 + index),
        colo: 'HKG',
        error: '',
        trace: '',
      },
    ]),
  );
  const at = Date.parse(backend.overview.sampled_at);
  await page.addInitScript(
    (history) =>
      localStorage.setItem('wwan-control.traffic.administrator', JSON.stringify(history)),
    {
      service: backend.overview.service_instance_id,
      at,
      baselines: Object.fromEntries(
        backend.overview.instances.map((instance) => [
          instance.id,
          {
            generation: instance.started_at,
            upload:
              (instance.metrics!.tcp_upload_bytes || 0) + (instance.metrics!.udp_upload_bytes || 0),
            download:
              (instance.metrics!.tcp_download_bytes || 0) +
              (instance.metrics!.udp_download_bytes || 0),
          },
        ]),
      ),
      points: Array.from({ length: 60 }, (_, index) => ({
        at: at - (60 - index) * 4000,
        upload: 1024 * 1024 * (3 + Math.sin(index / 3)),
        download: 1024 * 1024 * (8 + Math.cos(index / 4)),
      })),
    },
  );
  await page.reload();
  await expect(page.getByText('所有出口运行正常')).toBeVisible();
  await page.screenshot({
    path: path.join(docs, 'webui-overview.png'),
    fullPage: true,
    animations: 'disabled',
  });
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await expect(page.getByLabel('Vohive 设备编码')).toHaveValue('Y2');
  await expect(page.locator('.server-editor .ant-modal-title')).toHaveText(
    '编辑出口 · 移动网络 01',
  );
  await page.screenshot({
    path: path.join(docs, 'webui-editor.png'),
    fullPage: true,
    animations: 'disabled',
  });
  await page.getByRole('button', { name: '取消', exact: true }).click();
  await page.getByRole('combobox', { name: '界面主题' }).click();
  await page
    .locator('.ant-select-item-option-content')
    .getByText('深色主题', { exact: true })
    .click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  await page.screenshot({
    path: path.join(docs, 'webui-overview-dark.png'),
    fullPage: true,
    animations: 'disabled',
  });
  await page.getByRole('combobox', { name: '界面主题' }).click();
  await page
    .locator('.ant-select-item-option-content')
    .getByText('浅色主题', { exact: true })
    .click();
  await page.getByRole('menuitem', { name: '系统设置' }).click();
  await expect(page.getByLabel('Vohive 用户名')).toHaveValue('admin');
  await page.screenshot({
    path: path.join(docs, 'webui-settings.png'),
    fullPage: true,
    animations: 'disabled',
  });
  for (const [key, label, ready] of [
    ['configuration', '连接配置', '.ant-table-row'],
    ['performance', '实时性能', 'canvas'],
    ['logs', '运行日志', '.ant-table-row'],
    ['events', 'Vohive 事件', '.ant-table-row'],
    ['statistics', '历史统计', 'canvas'],
  ]) {
    await page.goto('/#' + key);
    await expect(page.getByRole('heading', { name: label, exact: true })).toBeVisible();
    await expect(page.locator(ready).first()).toBeVisible();
    await page.screenshot({
      path: path.join(docs, 'webui-' + key + '.png'),
      fullPage: true,
      animations: 'disabled',
    });
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/#overview');
  await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();
  await page.screenshot({
    path: path.join(docs, 'webui-mobile.png'),
    fullPage: true,
    animations: 'disabled',
  });
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await expect(page.getByLabel('Vohive 设备编码')).toBeVisible();
  await expect(page.locator('.server-editor .ant-modal-title')).toHaveText(
    '编辑出口 · 移动网络 01',
  );
  await page.screenshot({
    path: path.join(docs, 'webui-mobile-editor.png'),
    fullPage: false,
    animations: 'disabled',
  });
  await page.getByRole('button', { name: '取消', exact: true }).click();
  await page.goto('/#configuration');
  await expect(page.getByRole('button', { name: '编辑', exact: true }).first()).toBeVisible();
  await page.screenshot({
    path: path.join(docs, 'webui-mobile-connections.png'),
    fullPage: true,
    animations: 'disabled',
  });
});
