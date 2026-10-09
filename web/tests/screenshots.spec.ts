import { test, expect } from './fixtures';
import path from 'node:path';
import { seedDemoData } from './demo';

test('生成 Ant Design 页面预览', async ({ page, backend }) => {
  test.setTimeout(120000);
  await page.setViewportSize({ width: 1440, height: 1000 });
  const docs =
    process.env.WWAN_UI_SCREENSHOT_DIR || path.resolve(import.meta.dirname, '../../docs');
  backend.auth = { initialized: false, authenticated: false };
  await page.goto('/');
  await expect(page.getByRole('button', { name: '初始化并进入控制台' })).toBeVisible();
  await page.screenshot({
    path: path.join(docs, 'webui-initialization.png'),
    fullPage: true,
    animations: 'disabled',
  });
  await seedDemoData(page, backend);
  await page.reload();
  await expect(page.getByText('部分出口需要关注')).toBeVisible();
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
  await page.mouse.move(1, 1);
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
  if (process.env.WWAN_UI_AUDIT) {
    const audit = process.env.WWAN_UI_AUDIT_DIR || '/tmp/wwan-ui-audit';
    for (const width of [390, 1440]) {
      for (const dark of [false, true]) {
        await page.setViewportSize({ width, height: width === 390 ? 844 : 1000 });
        await page.getByRole('combobox', { name: '界面主题' }).click();
        await page
          .locator('.ant-select-item-option-content')
          .getByText(dark ? '深色主题' : '浅色主题', { exact: true })
          .click();
        for (const [key, label, ready] of [
          ['overview', '网络总览', '.egress-card'],
          ['configuration', '连接配置', width < 768 ? '.mobile-record' : '.ant-table-row'],
          ['performance', '实时性能', 'canvas'],
          ['logs', '运行日志', width < 768 ? '.mobile-record' : '.ant-table-row'],
          ['events', 'Vohive 事件', width < 768 ? '.mobile-record' : '.ant-table-row'],
          ['statistics', '历史统计', 'canvas'],
          ['settings', '系统设置', '.sessions-card .ant-list-item'],
        ]) {
          await page.goto('/#' + key);
          await expect(page.getByRole('heading', { name: label, exact: true })).toBeVisible();
          await expect(page.locator(ready).first()).toBeVisible();
          await page.screenshot({
            path: path.join(audit, 'demo-' + width + (dark ? '-dark' : ''), key + '.png'),
            fullPage: true,
            animations: 'disabled',
          });
        }
      }
    }
  }
});
