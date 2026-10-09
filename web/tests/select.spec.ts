import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

async function selectWithPointer(page: Page, name: string, option: string) {
  const input = page.getByRole('combobox', { name, exact: true });
  const select = page.locator('.ant-select').filter({ has: input });
  await select.scrollIntoViewIfNeeded();
  const bounds = await select.boundingBox();
  const x = bounds!.x + bounds!.width - 16;
  const y = bounds!.y + bounds!.height / 2;
  const touch = await page.evaluate(() => navigator.maxTouchPoints > 0);
  if (touch) await page.touchscreen.tap(x, y);
  else await page.mouse.click(x, y);
  await expect(input).toHaveAttribute('aria-expanded', 'true');
  // Leave the menu open across several live snapshots before selecting an option.
  await page.waitForTimeout(600);
  await expect(input).toHaveAttribute('aria-expanded', 'true');
  const popup = page.locator('.ant-select-dropdown:visible');
  await expect(popup).toHaveCount(1);
  const item = popup.locator('.ant-select-item-option-content').getByText(option, { exact: true });
  await expect(item).toBeInViewport();
  if (touch) await item.tap();
  else await item.click();
  await expect(input).toHaveAttribute('aria-expanded', 'false');
}

for (const reducedMotion of ['no-preference', 'reduce'] as const) {
  test(`系统动画偏好从 ${reducedMotion} 切换时保留编辑草稿`, async ({ page, backend }) => {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ reducedMotion });
    await page.goto('/');
    await page.getByRole('button', { name: '编辑 ' + backend.overview.servers[0].name }).click();
    await page.getByLabel('出口名称').fill('尚未保存的动画偏好测试');
    await page.emulateMedia({
      reducedMotion: reducedMotion === 'reduce' ? 'no-preference' : 'reduce',
    });
    await expect(page.getByRole('dialog')).toBeVisible();
    await expect(page.getByLabel('出口名称')).toHaveValue('尚未保存的动画偏好测试');
    await page.getByRole('tab', { name: 'DNS 解析', exact: true }).click();
    await selectWithPointer(page, 'DNS 模式', 'DNS over HTTPS');
    await expect(page.getByLabel('DoH 端点')).toBeVisible();
  });
}

for (const width of [390, 1440]) {
  for (const reducedMotion of ['no-preference', 'reduce'] as const) {
    const motion = reducedMotion === 'reduce' ? '减少动画' : '正常动画';
    test(`下拉菜单在 ${width}px ${motion}下支持点击、选择和持续实时更新`, async ({
      page,
      backend,
    }, testInfo) => {
      test.skip(width === 1440 && testInfo.project.name.startsWith('mobile-'));
      test.setTimeout(60000);
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ reducedMotion });
      const timer = setInterval(() => {
        backend.overview.uptime_seconds++;
        backend.push();
      }, 100);
      try {
        await page.goto('/');
        await selectWithPointer(page, '界面主题', '深色主题');
        await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
        await selectWithPointer(page, '界面主题', '浅色主题');
        await page.goto('/#logs');
        await selectWithPointer(page, '日志级别', 'ERROR');
        await expect
          .poll(() => backend.requests.some((request) => request.path.includes('level=ERROR')))
          .toBe(true);
        await page.goto('/#events');
        await selectWithPointer(page, '事件类型', '恢复成功');
        await page.goto('/#statistics');
        await selectWithPointer(page, '统计出口', '移动网络 01');
        await expect
          .poll(() => backend.requests.some((request) => request.path.includes('server_id=1')))
          .toBe(true);
        await page.goto('/#settings');
        await selectWithPointer(page, '最低日志级别', 'INFO');
        await selectWithPointer(page, '更新下载网口', 'lo');
        await page.goto('/');
        await page.getByRole('button', { name: '编辑 移动网络 01' }).click();
        await page.getByLabel('出口网口', { exact: true }).click();
        await expect(
          page.locator('.ant-select-dropdown:visible').getByText('lo · 127.0.0.1/8'),
        ).toBeVisible();
        await page.keyboard.press('Escape');
        const section = async (name: string) => {
          if (width < 768)
            await page.locator('.editor-section-selector').getByText(name, { exact: true }).click();
          else await page.getByRole('tab', { name, exact: true }).click();
        };
        await section('协议与认证');
        await selectWithPointer(page, '代理认证方式', '无需认证');
        await page.getByRole('switch', { name: '启用上游代理' }).click();
        await selectWithPointer(page, '上游认证', '用户名与密码');
        await section('访问控制');
        await selectWithPointer(page, '目标默认动作', '拒绝');
        await section('DNS 解析');
        await selectWithPointer(page, 'DNS 模式', 'DNS over HTTPS');
        await expect(page.getByLabel('DoH 端点')).toBeVisible();
      } finally {
        clearInterval(timer);
      }
    });
  }
}
