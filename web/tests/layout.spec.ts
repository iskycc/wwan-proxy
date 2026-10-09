import { test, expect } from './fixtures';
import type { Backend } from './fixtures';
import type { Page } from '@playwright/test';
import path from 'node:path';

const viewports = [
  { width: 320, height: 720 },
  { width: 390, height: 844 },
  { width: 768, height: 1024 },
  { width: 1024, height: 768 },
  { width: 1440, height: 1000 },
  { width: 2560, height: 1440 },
];
const pages = [
  ['overview', '网络总览'],
  ['configuration', '连接配置'],
  ['performance', '实时性能'],
  ['logs', '运行日志'],
  ['events', 'Vohive 事件'],
  ['statistics', '历史统计'],
  ['settings', '系统设置'],
];

async function populate(page: Page, backend: Backend) {
  backend.auth.username = ('administrator-' + 'long-account-name-'.repeat(3)).slice(0, 64);
  backend.update.current_version = '2026.10.08-' + 'abcdef1234'.repeat(4);
  backend.update.checked = true;
  backend.update.update_available = true;
  backend.update.latest = {
    tag: 'build-latest',
    version: '2026.10.09-' + '1234567890'.repeat(4),
    url: 'https://github.com/iskycc/wwan-proxy/releases',
    published_at: new Date().toISOString(),
    asset_name: 'wwan-proxy.tar.gz',
  };
  const server = backend.overview.servers[0];
  server.enabled = true;
  server.name = '移动网络主出口与备用链路配置 ' + '非常长的连接名称'.repeat(5);
  server.listen = '[2001:db8:1234:5678:90ab:cdef:1234:5678]:1080';
  server.http_proxy = { enabled: true, listen: '[2001:db8::1234]:8080' };
  server.interface = 'wwan0-multiwan';
  server.dns = {
    ipv4_only: true,
    doh: {
      urls: ['https://dns.example.com/dns-query'],
      bootstrap_ips: ['1.1.1.1'],
      timeout: '10s',
      insecure_skip_verify: false,
    },
  };
  backend.overview.servers = Array.from({ length: 6 }, (_, index) => ({
    ...structuredClone(server),
    id: index + 1,
    vohive_device_id: 'Y' + (index + 1),
  }));
  backend.overview.instances = backend.overview.servers.map((server) => ({
    id: server.id!,
    name: server.name,
    enabled: true,
    running: true,
    http_running: true,
    listen: server.listen,
    interface: server.interface,
    started_at: new Date(Date.now() - 86400000).toISOString(),
    metrics: {
      active_connections: 15687,
      active_udp: 234,
      total_connections: 8527654,
      tcp_upload_bytes: 775557467889,
      tcp_download_bytes: 1597783557855,
    },
    http_metrics: { active_requests: 412, total_requests: 521154 },
  }));
  backend.overview.heartbeats = Object.fromEntries(
    backend.overview.servers.map((server, index) => [
      server.id,
      {
        checked_at: new Date().toISOString(),
        healthy: index < 4,
        latency_ms: 187,
        status_code: 200,
        public_ip: '2001:db8:1234:5678:90ab:cdef:1234:' + index,
        colo: 'HKG',
        trace: '',
        error: index < 4 ? '' : '连接失败：' + 'network-timeout-detail-'.repeat(15),
      },
    ]),
  );
  const reply = (value: unknown) => ({
    contentType: 'application/json',
    body: JSON.stringify(value),
  });
  await page.route('**/api/logs?**', (route) =>
    route.fulfill(
      reply(
        Array.from({ length: 35 }, (_, index) => ({
          id: index + 1,
          timestamp: new Date().toISOString(),
          level: index % 2 ? 'WARN' : 'ERROR',
          component: 'manager',
          server_name: server.name,
          message: '出口连接失败：' + 'tcp-dial-timeout-'.repeat(20),
          details: { error: 'upstream-connection-refused-'.repeat(20) },
        })),
      ),
    ),
  );
  await page.route('**/api/vohive/events?**', (route) =>
    route.fulfill(
      reply([
        {
          id: 1,
          type: 'degraded',
          device_id: 'Y1',
          created_at: new Date().toISOString(),
          message: 'health degraded',
          details: {
            devices: Object.fromEntries(
              Array.from({ length: 12 }, (_, index) => ['Y' + index, { healthy: index % 3 !== 0 }]),
            ),
          },
        },
      ]),
    ),
  );
  await page.route('**/api/sessions', (route) =>
    route.fulfill(
      reply(
        Array.from({ length: 3 }, (_, index) => ({
          id: String(index).repeat(64),
          current: index === 0,
          remote_addr: '[2001:db8:1234:5678::42]:53201',
          user_agent: 'Mozilla/5.0 ' + 'ExampleBrowser/123.4567890 '.repeat(15),
          created_at: new Date().toISOString(),
          expires_at: backend.auth.expires_at,
        })),
      ),
    ),
  );
  await page.route('**/api/stats?**', (route) => {
    const url = new URL(route.request().url());
    const from = Date.parse(url.searchParams.get('from')!),
      to = Date.parse(url.searchParams.get('to')!);
    return route.fulfill(
      reply(
        Array.from({ length: 72 }, (_, index) => ({
          bucket: new Date(from + (index * (to - from)) / 71).toISOString(),
          upload_bytes: 1024 * 1024 * (10 + 4 * Math.sin(index / 4)),
          download_bytes: 1024 * 1024 * (18 + 6 * Math.cos(index / 6)),
          heartbeat_latency_ms: 100 + 40 * Math.sin(index / 8),
          heartbeat_healthy: index % 7 !== 0,
          success_rate: 0.92 + (index % 8) / 100,
        })),
      ),
    );
  });
  await page.route('**/api/stats/summary?**', (route) =>
    route.fulfill(
      reply({
        upload_bytes: 157845874355,
        download_bytes: 786456677822,
        avg_latency_ms: 187.1234567890123,
        success_rate: 0.98123456,
        peak_active_connections: 1000323,
        total_buckets: 72,
        healthy_buckets: 68,
      }),
    ),
  );
  const at = Date.parse(backend.overview.sampled_at);
  await page.addInitScript(
    ({ username, history }) =>
      localStorage.setItem('wwan-control.traffic.' + username, JSON.stringify(history)),
    {
      username: backend.auth.username,
      history: {
        service: backend.overview.service_instance_id,
        at: at - 1000,
        baselines: Object.fromEntries(
          backend.overview.instances.map((instance) => [
            instance.id,
            {
              generation: instance.started_at,
              upload: instance.metrics!.tcp_upload_bytes,
              download: instance.metrics!.tcp_download_bytes,
            },
          ]),
        ),
        points: Array.from({ length: 60 }, (_, index) => ({
          at: at - (60 - index) * 4000,
          upload: 1024 * 1024 * (3 + Math.sin(index / 3)),
          download: 1024 * 1024 * (8 + Math.cos(index / 4)),
        })),
      },
    },
  );
}

async function capture(page: Page, name: string, fullPage = true) {
  if (!process.env.WWAN_UI_AUDIT) return;
  await page.screenshot({
    path: path.join('/tmp/wwan-ui-audit', name + '.png'),
    fullPage,
    animations: 'disabled',
  });
}

async function geometry(page: Page) {
  return page.evaluate(() => {
    const issues: string[] = [];
    if (document.documentElement.scrollWidth > innerWidth + 1)
      issues.push('页面横向溢出 ' + document.documentElement.scrollWidth + '/' + innerWidth);
    const header = document.querySelector('.app-header');
    if (header)
      for (const child of header.children) {
        const rect = child.getBoundingClientRect(),
          parent = header.getBoundingClientRect();
        if (
          rect.left < parent.left - 1 ||
          rect.right > parent.right + 1 ||
          rect.top < parent.top - 1 ||
          rect.bottom > parent.bottom + 1
        )
          issues.push('顶部控件溢出 ' + child.className);
      }
    if (
      header &&
      header.children.length === 2 &&
      header.children[0].getBoundingClientRect().right >
        header.children[1].getBoundingClientRect().left + 1
    )
      issues.push('顶部导航与账户控件重叠');
    if (innerWidth < 768)
      for (const meta of document.querySelectorAll('.sessions-card .ant-list-item-meta')) {
        const parent = meta.parentElement!;
        if (meta.getBoundingClientRect().width < parent.clientWidth - 2)
          issues.push('登录设备信息被操作按钮挤压');
      }
    for (const element of document.querySelectorAll<HTMLElement>(
      '.ant-statistic-content, .ant-card-head, .filter-bar, .statistics-toolbar, .mobile-record, .dynamic-row, .session-description',
    )) {
      if (!element.getClientRects().length) continue;
      if (element.scrollWidth > element.clientWidth + 2)
        issues.push(
          element.className + ' 内容溢出 ' + element.scrollWidth + '/' + element.clientWidth,
        );
    }
    return issues;
  });
}

for (const viewport of [
  ...viewports.map((v) => ({ ...v, dark: false })),
  ...viewports.filter((v) => [320, 768, 1440].includes(v.width)).map((v) => ({ ...v, dark: true })),
]) {
  test(
    '全部页面在 ' +
      viewport.width +
      'px ' +
      (viewport.dark ? '深色' : '浅色') +
      '下保持布局，长数据不溢出',
    async ({ page, backend }) => {
      test.setTimeout(90000);
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: viewport.dark ? 'dark' : 'light' });
      await populate(page, backend);
      for (const [key, label] of pages) {
        await page.goto('/#' + key);
        await expect(page.getByRole('heading', { name: label, exact: true })).toBeVisible();
        await expect(page.locator('.page-skeleton')).toHaveCount(0);
        if (key === 'settings') await expect(page.getByLabel('Vohive 用户名')).toBeVisible();
        if (key === 'logs' || key === 'events')
          await expect(
            page
              .locator(
                viewport.width < 768
                  ? '.mobile-records .ant-list-item'
                  : '.ant-table-tbody > .ant-table-row',
              )
              .first(),
          ).toBeVisible();
        if (key === 'performance') await expect(page.locator('canvas').first()).toBeVisible();
        if (key === 'statistics') await expect(page.locator('canvas').first()).toBeVisible();
        await capture(page, viewport.width + (viewport.dark ? '-dark' : '') + '/' + key);
        expect.soft(await geometry(page), label + ' · ' + viewport.width).toEqual([]);
      }
    },
  );
}

for (const viewport of [
  ...viewports.slice(0, 5).map((v) => ({ ...v, dark: false })),
  ...viewports.filter((v) => [320, 1440].includes(v.width)).map((v) => ({ ...v, dark: true })),
]) {
  test(
    '登录与初始化表单在 ' + viewport.width + 'px ' + (viewport.dark ? '深色' : '浅色') + '下可用',
    async ({ page, backend }) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: viewport.dark ? 'dark' : 'light' });
      backend.auth = { initialized: false, authenticated: false };
      await page.goto('/');
      await expect(page.getByRole('button', { name: '初始化并进入控制台' })).toBeVisible();
      await capture(page, viewport.width + (viewport.dark ? '-dark' : '') + '/initialize');
      expect.soft(await geometry(page)).toEqual([]);
      await page.getByRole('button', { name: '初始化并进入控制台' }).click();
      await expect(page.getByText('请输入用户名')).toBeVisible();
      expect.soft(await geometry(page)).toEqual([]);
      backend.auth.initialized = true;
      await page.reload();
      await expect(page.getByRole('heading', { name: '欢迎回来' })).toBeVisible();
      await capture(page, viewport.width + (viewport.dark ? '-dark' : '') + '/login');
      expect.soft(await geometry(page)).toEqual([]);
    },
  );
}

async function selectSection(page: Page, label: string) {
  if (page.viewportSize()!.width < 768) {
    await page.locator('.editor-section-selector').getByText(label, { exact: true }).click();
    await expect(page.getByRole('radio', { name: label, exact: true })).toBeChecked();
  } else await page.getByRole('tab', { name: label, exact: true }).click();
}

for (const viewport of [...viewports.slice(0, 3), viewports[4], { width: 844, height: 390 }]) {
  test(
    '连接编辑全部分组、动态行和错误定位在 ' + viewport.width + '×' + viewport.height + ' 下可用',
    async ({ page, backend }) => {
      test.setTimeout(90000);
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: viewport.width === 320 ? 'dark' : 'light' });
      await populate(page, backend);
      const server = backend.overview.servers[0];
      server.upstream = {
        enabled: true,
        address: 'upstream.example.com:1080',
        auth_method: 'username_password',
        username: 'proxy-admin',
        password: '',
      };
      server.bind = { enabled: true, advertise: 'auto' };
      server.auth = {
        method: 'username_password',
        users: { alice: '', bob: '' },
        password_unchanged: ['alice', 'bob'],
      };
      server.dns.doh!.headers = {
        Authorization: 'Bearer example-long-key-for-layout-check',
        'X-Client': 'wwan-proxy',
      };
      server.udp.advertise_map = { '10.0.0.1': '2001:db8:1234:5678::1', '10.0.0.2': '203.0.113.2' };
      server.udp.advertise_source_map = {
        '192.168.0.0/16': '203.0.113.1',
        '2001:db8:1234::/48': '2001:db8:4567::1',
      };
      await page.goto('/#configuration');
      await page.getByRole('button', { name: '编辑', exact: true }).first().click();
      await expect(page.getByLabel('Vohive 设备编码')).toHaveValue('Y1');
      const dialog = page.getByRole('dialog');
      for (const [label, field] of [
        ['基本设置', '出口名称'],
        ['协议与认证', '代理认证方式'],
        ['访问控制', '允许访问的来源网段'],
        ['DNS 解析', 'DNS 模式'],
        ['UDP 中继', '启用 UDP ASSOCIATE'],
        ['出口心跳', '心跳 URL'],
      ]) {
        if (label !== '基本设置') await selectSection(page, label);
        await expect(page.getByLabel(field, { exact: true })).toBeVisible();
        await expect(page.getByRole('button', { name: '保存并应用' })).toBeInViewport({ ratio: 1 });
        const box = await dialog.boundingBox();
        expect.soft(box!.x).toBeGreaterThanOrEqual(0);
        expect.soft(box!.y).toBeGreaterThanOrEqual(0);
        expect.soft(box!.x + box!.width).toBeLessThanOrEqual(viewport.width + 1);
        expect.soft(box!.y + box!.height).toBeLessThanOrEqual(viewport.height + 1);
        expect.soft(await geometry(page), label).toEqual([]);
        await capture(page, viewport.width + 'x' + viewport.height + '/editor-' + label, false);
      }
      await selectSection(page, '基本设置');
      await expect(page.getByLabel('出口名称')).toHaveValue(server.name);
      await selectSection(page, 'UDP 中继');
      await page.getByLabel('固定 Relay 端口池').fill('12000,12000');
      await selectSection(page, '出口心跳');
      await page.getByRole('button', { name: '保存并应用' }).click();
      await expect(page.getByText('UDP 固定端口不能重复')).toBeVisible();
      await expect(page.getByLabel('固定 Relay 端口池')).toBeFocused();
      await expect(page.getByLabel('固定 Relay 端口池')).toBeInViewport({ ratio: 1 });
      await page.getByLabel('固定 Relay 端口池').fill('12000,12007');
      await selectSection(page, '协议与认证');
      await page.getByRole('button', { name: '添加认证用户' }).click();
      await selectSection(page, '出口心跳');
      await page.getByRole('button', { name: '保存并应用' }).click();
      await expect(page.getByLabel('代理用户名').last()).toBeFocused();
      await expect(page.getByLabel('代理用户名').last()).toBeInViewport({ ratio: 1 });
      await capture(page, viewport.width + 'x' + viewport.height + '/editor-validation', false);
      await page.getByLabel('代理用户名').last().fill('new-user');
      await page.getByLabel('代理用户密码').last().fill('test-password');
      await page.getByRole('button', { name: '保存并应用' }).click();
      await expect(dialog).toBeHidden();
      const saved = backend.requests.find(
        (request) => request.method === 'PUT' && request.path === '/api/servers/1',
      )!.body;
      expect(saved.auth.users['new-user']).toBe('test-password');
      expect(saved.auth.password_unchanged).toEqual(['alice', 'bob']);
      expect(saved.udp.advertise_map).toEqual(server.udp.advertise_map);
      expect(saved.dns.doh.headers).toEqual(server.dns.doh!.headers);
    },
  );
}

test('桌面日志分页可更改每页条数，完整消息可以展开', async ({ page, backend }) => {
  await populate(page, backend);
  await page.goto('/#logs');
  await expect(page.locator('.ant-table-row')).toHaveCount(20);
  await page.locator('.ant-pagination-options-size-changer').click();
  await page
    .locator('.ant-select-item-option-content')
    .getByText('50 条/页', { exact: true })
    .click();
  await expect(page.locator('.ant-table-row')).toHaveCount(35);
  await page.getByRole('button', { name: '展开行' }).first().click();
  await expect(page.locator('.detail-json').first()).toContainText('tcp-dial-timeout-'.repeat(20));
});

for (const width of [320, 1440]) {
  test('空数据、加载失败与恢复在 ' + width + 'px 下可读', async ({ page, backend }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.emulateMedia({ colorScheme: 'dark' });
    backend.overview.servers = [];
    await page.goto('/');
    await expect(page.getByText('尚未创建出口，请点击右上角「新建出口」。')).toBeVisible();
    expect(await geometry(page)).toEqual([]);
    const error = '查询失败，请稍后重试：' + 'upstream-timeout-'.repeat(20);
    await page.route('**/api/logs?**', (route) =>
      route.fulfill({
        status: 503,
        contentType: 'application/json',
        body: JSON.stringify({ error }),
      }),
    );
    await page.goto('/#logs');
    await expect(page.getByText(error)).toBeVisible();
    await capture(page, width + '/logs-error');
    expect(await geometry(page)).toEqual([]);
    await page.unroute('**/api/logs?**');
    await page.getByRole('button', { name: '刷新日志', exact: true }).click();
    await expect(page.getByText('测试心跳错误')).toBeVisible();
    backend.settingsError = true;
    await page.goto('/#settings');
    await expect(page.getByText('settings unavailable')).toBeVisible();
    expect(await geometry(page)).toEqual([]);
    backend.settingsError = false;
    await page.getByRole('button', { name: '重试系统设置', exact: true }).click();
    await expect(page.getByLabel('Vohive 用户名')).toBeVisible();
  });
}

for (const width of [320, 768, 1440]) {
  test('导航、记录详情、翻页和确认弹窗在 ' + width + 'px 下正常', async ({ page, backend }) => {
    test.setTimeout(60000);
    await page.setViewportSize({ width, height: 900 });
    await populate(page, backend);
    await page.goto('/');
    if (width < 992) await page.getByRole('button', { name: '打开导航菜单' }).click();
    await page.getByRole('menuitem', { name: '运行日志' }).click();
    await expect(page.getByRole('heading', { name: '运行日志', exact: true })).toBeVisible();
    if (width < 768) await page.getByText('查看详情', { exact: true }).first().click();
    else await page.getByRole('button', { name: '展开行' }).first().click();
    await expect(page.locator('.detail-json').first()).toContainText(
      'tcp-dial-timeout-'.repeat(20),
    );
    expect(await geometry(page)).toEqual([]);
    await page.locator('.ant-pagination-next').click();
    await expect(
      page.locator(width < 768 ? '.mobile-records .ant-list-item' : '.ant-table-row'),
    ).toHaveCount(15);
    await page.goto('/#events');
    if (width < 768) await page.getByText('查看详情', { exact: true }).first().click();
    else await page.getByRole('button', { name: '展开行' }).first().click();
    await expect(page.locator('.detail-json').first()).toContainText('health degraded');
    expect(await geometry(page)).toEqual([]);
    await page.goto('/#settings');
    for (const [button, title] of [
      ['撤销其他设备', '撤销其他登录设备'],
      ['安装更新', '安装程序更新'],
    ]) {
      await page.getByRole('button', { name: button, exact: true }).click();
      const confirm = page.getByRole('dialog', { name: title, exact: true });
      await expect(confirm).toBeVisible();
      const bounds = await confirm.boundingBox();
      expect(bounds!.x).toBeGreaterThanOrEqual(0);
      expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
      await expect(confirm.getByRole('button', { name: /取\s*消/ })).toBeInViewport({ ratio: 1 });
      await capture(page, width + '/confirm-' + title, false);
      await confirm.getByRole('button', { name: /取\s*消/ }).click();
      await expect(confirm).toBeHidden();
    }
    expect(
      backend.requests.some(
        (request) => request.method === 'POST' && request.path === '/api/update',
      ),
    ).toBe(false);
    await page.evaluate(() => window.scrollTo({ top: document.documentElement.scrollHeight }));
    if (width < 992) await page.getByRole('button', { name: '打开导航菜单' }).click();
    await page.getByRole('menuitem', { name: '网络总览' }).click();
    await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeInViewport({
      ratio: 1,
    });
    await expect.poll(() => page.evaluate(() => scrollY)).toBe(0);
    await page.goto('/#settings');
    await page.getByRole('button', { name: '退出当前', exact: true }).click();
    await page
      .getByRole('dialog', { name: '退出当前会话' })
      .getByRole('button', { name: /确\s*定/ })
      .click();
    await expect(page.getByRole('heading', { name: '欢迎回来' })).toBeVisible();
  });
}
