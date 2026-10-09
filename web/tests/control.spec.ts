import { test, expect } from './fixtures';

test('首次初始化与重新登录使用 Ant Design 表单', async ({ page, backend }) => {
  backend.auth = { initialized: false, authenticated: false };
  await page.goto('/');
  await page.getByLabel('管理员用户名').fill('test-admin');
  await page.getByLabel('管理员密码', { exact: true }).fill('StrongPassword!42');
  await page.getByLabel('确认密码', { exact: true }).fill('DifferentPassword!42');
  await page.getByRole('button', { name: '初始化并进入控制台' }).click();
  await expect(page.getByText('两次输入的密码不一致')).toBeVisible();
  expect(backend.requests.some((request) => request.path === '/api/auth/initialize')).toBe(false);
  await page.getByLabel('确认密码', { exact: true }).fill('StrongPassword!42');
  await page.getByRole('button', { name: '初始化并进入控制台' }).click();
  await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();
  expect(backend.requests.find((request) => request.path === '/api/auth/initialize')?.body).toEqual(
    { username: 'test-admin', password: 'StrongPassword!42' },
  );
  backend.auth.authenticated = false;
  await page.reload();
  await expect(page.getByRole('heading', { name: '欢迎回来' })).toBeVisible();
  await page.getByLabel('管理员用户名').fill('test-admin');
  await page.getByLabel('管理员密码').fill('StrongPassword!42');
  await page.getByRole('button', { name: '登录控制台', exact: true }).click();
  await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();
});

test('直接编辑会加载 Vohive 编码并保留代理密码与 UDP 端口池', async ({ page, backend }) => {
  backend.settingsDelay = 120;
  await page.goto('/');
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await expect(page.getByLabel('Vohive 设备编码')).toHaveValue('Y2');
  await page.getByLabel('Vohive 设备编码').fill('Y9');
  await page.getByRole('button', { name: '保存并应用' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  const saved = backend.requests.find(
    (request) => request.method === 'PUT' && request.path === '/api/servers/1',
  )?.body;
  expect(saved.vohive_device_id).toBe('Y9');
  expect(saved.auth).toEqual({
    method: 'username_password',
    users: { alice: '' },
    password_unchanged: ['alice'],
  });
  expect(saved.udp.relay_ports).toEqual([12000, 12007]);
  expect(saved.upstream.password).toBe('');
  expect(
    backend.requests.some(
      (request) => request.path === '/api/settings' && request.method === 'PUT',
    ),
  ).toBe(false);
  await page.reload();
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await expect(page.getByLabel('Vohive 设备编码')).toHaveValue('Y9');
});

test('读取设置失败可重试，Vohive 关闭时保留已有设备编码', async ({ page, backend }) => {
  backend.settingsError = true;
  await page.goto('/');
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await expect(page.getByText('读取系统设置失败：settings unavailable')).toBeVisible();
  await expect(page.getByRole('dialog')).toBeHidden();
  backend.settingsError = false;
  backend.settings.vohive.enabled = false;
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await expect(page.getByRole('dialog')).toBeVisible();
  await expect(page.getByLabel('Vohive 设备编码')).toHaveCount(0);
  await page.getByRole('button', { name: '保存并应用' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  expect(backend.requests.find((request) => request.method === 'PUT')?.body.vohive_device_id).toBe(
    'Y2',
  );
});

test('实时更新不会覆盖编辑草稿，星号密码作为新字面密码提交', async ({ page, backend }) => {
  await page.goto('/');
  await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
  await page.getByLabel('出口名称').fill('编辑中的名称');
  backend.overview.servers[0].name = '远端更新名称';
  backend.push();
  await expect(page.getByLabel('出口名称')).toHaveValue('编辑中的名称');
  await page.getByRole('tab', { name: '协议与认证' }).click();
  await page.getByRole('textbox', { name: '代理用户密码' }).fill('********');
  await page.getByRole('button', { name: '保存并应用' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  const saved = backend.requests.find((request) => request.method === 'PUT')?.body;
  expect(saved.name).toBe('编辑中的名称');
  expect(saved.auth.users.alice).toBe('********');
  expect(saved.auth.password_unchanged).toEqual([]);
});

test('新建、启停和删除出口通过对应 API 完成', async ({ page, backend }) => {
  await page.goto('/#configuration');
  await page.getByRole('button', { name: '启用', exact: true }).click();
  await expect(page.getByRole('button', { name: '停用', exact: true })).toBeVisible();
  await page.getByRole('button', { name: '新建出口' }).click();
  await page.getByLabel('出口名称').fill('新增出口');
  await page.getByLabel('出口网口', { exact: true }).fill('lo');
  await page.getByRole('button', { name: '保存并应用' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  expect(
    backend.requests.find((request) => request.method === 'POST' && request.path === '/api/servers')
      ?.body.name,
  ).toBe('新增出口');
  await page.getByRole('button', { name: '编辑', exact: true }).first().click();
  await page.getByRole('button', { name: '删除配置', exact: true }).click();
  await page.route('**/api/servers/1', (route) =>
    route.fulfill({
      status: 503,
      contentType: 'application/json',
      body: JSON.stringify({ error: '配置删除暂时失败，请重试' }),
    }),
  );
  await page
    .getByRole('dialog', { name: '删除出口配置', exact: true })
    .getByRole('button', { name: '删除配置', exact: true })
    .click();
  await expect(page.getByText('配置删除暂时失败，请重试')).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveCount(2);
  await page.unroute('**/api/servers/1');
  await page
    .getByRole('dialog', { name: '删除出口配置', exact: true })
    .getByRole('button', { name: '删除配置', exact: true })
    .click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  expect(
    backend.requests.some(
      (request) => request.method === 'DELETE' && request.path === '/api/servers/1',
    ),
  ).toBe(true);
});

test('日志、事件、图表和统计页面可用，过滤条件发送给服务端', async ({ page, backend }) => {
  await page.goto('/#logs');
  await expect(page.getByText('测试心跳错误')).toBeVisible();
  await page.getByPlaceholder('搜索消息、实例或错误').fill('测试');
  await page.getByPlaceholder('搜索消息、实例或错误').press('Enter');
  await expect
    .poll(() =>
      backend.requests.some(
        (request) =>
          request.path.includes('/api/logs?') && request.path.includes('q=%E6%B5%8B%E8%AF%95'),
      ),
    )
    .toBe(true);
  await page.getByRole('menuitem', { name: 'Vohive 事件' }).click();
  await expect(page.getByText('设备健康检查异常')).toBeVisible();
  await expect(page.getByText('Y2 异常')).toBeVisible();
  await page.getByRole('menuitem', { name: '历史统计' }).click();
  await expect(page.getByText('98.00%')).toBeVisible();
  await expect(page.locator('canvas').first()).toBeVisible();
  await page.getByText('最近 7 天', { exact: true }).click();
  await expect
    .poll(() =>
      backend.requests.some(
        (request) => request.path.startsWith('/api/stats?') && request.path.includes('step=hour'),
      ),
    )
    .toBe(true);
  await page.getByRole('menuitem', { name: '实时性能' }).click();
  await expect(page.getByText('UDP 丢弃 / 错误')).toBeVisible();
});

test('系统设置、管理员凭据和更新网口遵守 API 协议', async ({ page, backend }) => {
  await page.goto('/#settings');
  await page.getByLabel('登录会话有效期').fill('48h');
  await page.getByRole('button', { name: '保存系统设置' }).click();
  await expect(page.getByText('部分设置已变更，请安全重启 wwan-proxy 服务。')).toBeVisible();
  const settings = backend.requests.find(
    (request) => request.method === 'PUT' && request.path === '/api/settings',
  )?.body;
  expect(settings.session_lifetime).toBe('48h');
  expect(settings).not.toHaveProperty('current_database_path');
  await page.getByLabel('管理员用户名').fill('new-admin');
  await page.getByLabel('当前密码', { exact: true }).fill('StrongPassword!42');
  await page.getByRole('button', { name: '更新管理员信息' }).click();
  await expect
    .poll(() =>
      backend.requests.some(
        (request) => request.path === '/api/admin' && request.body.username === 'new-admin',
      ),
    )
    .toBe(true);
  await page.getByRole('combobox', { name: '更新下载网口' }).click();
  await page.locator('.ant-select-item-option-content').getByText('lo', { exact: true }).click();
  await page.getByRole('button', { name: '检查更新', exact: true }).click();
  await expect(page.getByText('new-version', { exact: true })).toBeVisible();
  expect(
    backend.requests.some((request) => request.path === '/api/update?refresh=1&interface=lo'),
  ).toBe(true);
  await page.getByRole('button', { name: '安装更新', exact: true }).click();
  await page.getByRole('button', { name: '开始更新', exact: true }).click();
  await expect
    .poll(
      () =>
        backend.requests.find(
          (request) => request.path === '/api/update' && request.method === 'POST',
        )?.body,
    )
    .toEqual({ interface: 'lo' });
});

test('会话撤销后返回登录页', async ({ page, backend }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();
  backend.auth.authenticated = false;
  backend.sockets[0].close({ code: 1008, reason: 'session expired' });
  await expect(page.getByRole('heading', { name: '欢迎回来' })).toBeVisible();
  await expect(page.getByRole('button', { name: '新建出口' })).toHaveCount(0);
});

test('手机导航、配置弹窗和主题切换可用且页面不横向溢出', async ({ page, backend }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');
  await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();
  await page.getByRole('button', { name: '打开导航菜单' }).click();
  await page.getByRole('menuitem', { name: '连接配置' }).click();
  await expect(page.getByRole('heading', { name: '连接配置', exact: true })).toBeVisible();
  await page.getByRole('button', { name: '编辑', exact: true }).click();
  await expect(page.getByLabel('Vohive 设备编码')).toHaveValue('Y2');
  await expect(page.getByRole('button', { name: '保存并应用' })).toBeInViewport();
  await page.getByRole('button', { name: '取消', exact: true }).click();
  await page.getByRole('combobox', { name: '界面主题' }).click();
  await page
    .locator('.ant-select-item-option-content')
    .getByText('深色主题', { exact: true })
    .click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(
    true,
  );
});
