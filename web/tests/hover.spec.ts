import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// Keep classic scrollbars visible so floating content cannot hide layout shifts.
test.use({
  launchOptions: {
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH,
    ignoreDefaultArgs: ['--hide-scrollbars'],
    args: ['--disable-features=OverlayScrollbar'],
  },
});

function observeLayout(page: Page) {
  return page.evaluate(
    () =>
      new Promise<number[][]>((resolve) => {
        const frames: number[][] = [];
        const started = performance.now();
        const sample = () => {
          frames.push([
            document.documentElement.clientWidth,
            document.documentElement.scrollHeight,
            scrollX,
            scrollY,
            ...['.app-layout', '.app-header', '.page-heading', '.metric-card'].flatMap(
              (selector) => {
                const rect = document.querySelector(selector)!.getBoundingClientRect();
                return [rect.x, rect.y, rect.width, rect.height];
              },
            ),
          ]);
          if (performance.now() - started < 1000) requestAnimationFrame(sample);
          else resolve(frames);
        };
        sample();
      }),
  );
}

function expectStableLayout(frames: number[][]) {
  expect(frames.length).toBeGreaterThan(10);
  for (let index = 0; index < frames[0].length; index++) {
    const values = frames.map((frame) => frame[index]);
    expect(Math.max(...values) - Math.min(...values)).toBeLessThanOrEqual(1);
  }
}

for (const [width, height] of [
  [390, 844],
  [1024, 720],
  [1440, 1200],
]) {
  for (const color of ['light', 'dark']) {
    test(`头像与流量悬停在 ${width}px ${color} 下不创建浮层或改变页面布局`, async ({
      page,
      backend,
    }) => {
      await page.setViewportSize({ width, height });
      backend.auth.username = 'administrator-' + 'username'.repeat(6);
      backend.overview.instances = [
        {
          id: 1,
          name: '移动网络 01',
          enabled: true,
          running: true,
          http_running: false,
          listen: '0.0.0.0:1080',
          interface: 'lo',
          metrics: {
            total_connections: 123456,
            tcp_upload_bytes: 1048576,
            tcp_download_bytes: 2097152,
          },
          http_metrics: {},
        },
      ];
      await page.addInitScript((color) => localStorage.setItem('wwan-control.theme', color), color);
      await page.goto('/');
      await expect(page.locator('.metric-card')).toHaveCount(4);
      await expect(page.locator('html')).toHaveAttribute('data-theme', color);
      await expect(page.getByLabel('累计连接 / 请求：123,456', { exact: true })).toHaveAttribute(
        'title',
        '123,456',
      );
      const metrics = backend.overview.instances[0].metrics;
      const targets = [page.locator('.account-button .ant-avatar')];
      for (let index = 0; index < 4; index++)
        targets.push(page.locator('.metric-card').nth(index).locator('.ant-statistic-content'));
      for (const target of targets) {
        // Scroll first, then monitor while the pointer stays on the target.
        await target.scrollIntoViewIfNeeded();
        const frames = observeLayout(page);
        await target.hover();
        for (let index = 0; index < 4; index++) {
          metrics.total_connections++;
          metrics.tcp_upload_bytes += 1024;
          backend.push();
          await page.waitForTimeout(150);
        }
        await expect(page.locator('.ant-dropdown, .ant-tooltip')).toHaveCount(0);
        await expect(
          page.getByLabel(`累计连接 / 请求：${metrics.total_connections.toLocaleString('zh-CN')}`, {
            exact: true,
          }),
        ).toBeVisible();
        expectStableLayout(await frames);
      }

      const account = page.getByRole('button', { name: '账户菜单', exact: true });
      await account.scrollIntoViewIfNeeded();
      const menuFrames = observeLayout(page);
      await account.click();
      const menu = page.locator('.account-menu .ant-dropdown');
      await expect(menu.getByText(backend.auth.username, { exact: true })).toBeVisible();
      await expect(menu.getByRole('menuitem', { name: '退出登录' })).toBeVisible();
      const bounds = await menu.boundingBox();
      expect(bounds!.x).toBeGreaterThanOrEqual(0);
      expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
      expectStableLayout(await menuFrames);
      await page.mouse.move(1, 1);
      await expect(menu).toBeVisible();
      await page.getByRole('heading', { name: '网络总览', exact: true }).click();
      await expect(menu).toBeHidden();
      await account.focus();
      await page.keyboard.press('Enter');
      await expect(menu).toBeVisible();
      await menu.getByRole('menuitem', { name: '退出登录' }).click();
      await expect(page.getByRole('heading', { name: '欢迎回来' })).toBeVisible();
    });
  }
}
