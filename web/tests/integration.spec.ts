import { test, expect } from '@playwright/test';

test('真实 Go 与 SQLite 支持新界面的配置、凭据回显和会话生命周期', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const origin = 'http://127.0.0.1:4180';
  await page.goto('/');
  await page.getByLabel('管理员用户名').fill('integration-admin');
  await page.getByLabel('管理员密码', { exact: true }).fill('IntegrationPassword!42');
  await page.getByLabel('确认密码', { exact: true }).fill('IntegrationPassword!42');
  await page.getByRole('button', { name: '初始化并进入控制台' }).click();
  await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();

  const settings = await (await page.request.get('/api/settings')).json();
  const savedSettings = await page.request.put('/api/settings', {
    headers: { Origin: origin },
    data: {
      web_listen: settings.web_listen,
      database_path: settings.database_path,
      log_level: settings.log_level,
      log_retention_days: settings.log_retention_days,
      session_lifetime: settings.session_lifetime,
      vohive: {
        ...settings.vohive,
        enabled: true,
        base_url: 'http://127.0.0.1:9999',
        username: 'test',
        password: 'TestVohivePassword!42',
      },
    },
  });
  expect(savedSettings.ok(), await savedSettings.text()).toBe(true);
  await page.getByRole('button', { name: '新建出口' }).click();
  await page.getByLabel('出口名称').fill('真实接口测试');
  await page.getByLabel('出口网口', { exact: true }).fill('lo');
  await page.getByLabel('Vohive 设备编码').fill('Y42');
  await page.getByRole('switch', { name: '保存后启用此实例' }).click();
  await page.getByRole('tab', { name: '协议与认证' }).click();
  await page.getByLabel('代理认证方式').click();
  await page
    .locator('.ant-select-item-option-content')
    .getByText('用户名与密码', { exact: true })
    .click();
  await page.getByRole('button', { name: '添加认证用户' }).click();
  await page.getByRole('textbox', { name: '代理用户名' }).fill('alice');
  await page.getByRole('textbox', { name: '代理用户密码' }).fill('ProxyPassword!42');
  await page.getByRole('tab', { name: 'UDP 中继' }).click();
  await page.getByLabel('固定 Relay 端口池').fill('12000, 12007, 53000');
  await page.getByRole('button', { name: '保存并应用' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();

  await page.reload();
  await page.getByRole('button', { name: '编辑 真实接口测试' }).click();
  await expect(page.getByLabel('Vohive 设备编码')).toHaveValue('Y42');
  await page.getByLabel('出口名称').fill('真实接口测试已编辑');
  await page.getByRole('tab', { name: '协议与认证' }).click();
  await expect(page.getByRole('textbox', { name: '代理用户密码' })).toHaveValue('');
  await page.getByRole('tab', { name: 'UDP 中继' }).click();
  await expect(page.getByLabel('固定 Relay 端口池')).toHaveValue('12000, 12007, 53000');
  await page.getByRole('button', { name: '保存并应用' }).click();
  await expect(page.getByRole('dialog')).toBeHidden();
  const servers = await (await page.request.get('/api/servers')).json();
  expect(servers[0].vohive_device_id).toBe('Y42');
  expect(servers[0].auth.password_unchanged).toEqual(['alice']);
  expect(servers[0].udp.relay_ports).toEqual([12000, 12007, 53000]);

  await page.getByRole('menuitem', { name: '系统设置' }).click();
  await page.getByLabel('登录会话有效期').fill('48h');
  await page.getByRole('button', { name: '保存系统设置' }).click();
  await expect
    .poll(async () => (await (await page.request.get('/api/settings')).json()).session_lifetime)
    .toBe('48h0m0s');
  await expect(page.getByText('当前浏览器', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: '退出当前', exact: true }).click();
  await page.getByRole('button', { name: /确\s*定/ }).click();
  await expect(page.getByRole('heading', { name: '欢迎回来' })).toBeVisible();
  expect(errors).toEqual([]);
});
