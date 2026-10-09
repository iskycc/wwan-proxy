import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

async function scrollState(page: Page) {
  return page.evaluate(() =>
    [document.documentElement, document.body].map((element) => ({
      overflow: getComputedStyle(element).overflow,
      width: element.clientWidth,
    })),
  );
}

for (const width of [390, 1440]) {
  for (const reducedMotion of ['no-preference', 'reduce'] as const) {
    test(`浮层在 ${width}px ${reducedMotion} 下关闭后恢复焦点和页面滚动`, async ({
      page,
      backend,
      isMobile,
      browserName,
    }, testInfo) => {
      test.skip(width === 1440 && testInfo.project.name.startsWith('mobile-'));
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ reducedMotion });
      await page.goto('/');
      await expect(page.getByRole('heading', { name: '网络总览', exact: true })).toBeVisible();
      await expect(
        page.getByRole('button', { name: '编辑 ' + backend.overview.servers[0].name }),
      ).toBeVisible();
      const baseline = await scrollState(page);
      if (width < 992) {
        const navigation = page.getByRole('button', { name: '打开导航菜单' });
        await navigation.focus();
        await navigation.press('Enter');
        const drawer = page.getByRole('dialog', { name: 'WWAN Control', exact: true });
        await expect(drawer).toBeVisible();
        await expect(drawer.getByRole('menuitem', { name: '系统设置' })).toBeInViewport();
        await page.keyboard.press('Escape');
        await expect(drawer).toBeHidden();
        await expect(navigation).toBeFocused();
        await expect.poll(() => scrollState(page)).toEqual(baseline);
        // Reopen and navigate to make sure the dismissed drawer has no active mask.
        await navigation.click();
        await drawer.getByRole('menuitem', { name: '系统设置' }).click();
        await expect(drawer).toBeHidden();
      } else {
        await page.getByRole('menuitem', { name: '系统设置' }).click();
      }
      await expect(page.getByRole('heading', { name: '系统设置', exact: true })).toBeVisible();
      const revoke = page.getByRole('button', { name: '撤销其他设备', exact: true });
      await expect(revoke).toBeVisible();
      await expect(page.getByText('当前浏览器', { exact: true })).toBeVisible();
      const settingsScroll = await scrollState(page);
      for (const closeWith of ['keyboard', 'button']) {
        await revoke.focus();
        await revoke.press('Enter');
        const dialog = page.getByRole('dialog', { name: '撤销其他登录设备', exact: true });
        await expect(dialog).toBeVisible();
        await expect(dialog.getByRole('button', { name: /取\s*消/ })).toBeInViewport({ ratio: 1 });
        if (closeWith === 'keyboard') await page.keyboard.press('Escape');
        else await dialog.getByRole('button', { name: /取\s*消/ }).click();
        await expect(dialog).toBeHidden();
        await expect(revoke).toBeFocused();
        await expect.poll(() => scrollState(page)).toEqual(settingsScroll);
      }
      // Check scrolling after portal teardown as well as the restored lock styles.
      // Mobile WebKit has no wheel input API; use window scrolling in that project.
      await page.evaluate(() => window.scrollTo(0, 0));
      if (isMobile && browserName === 'webkit') {
        await page.evaluate(() => window.scrollBy(0, 400));
      } else {
        await page.mouse.move(width / 2, 500);
        await page.mouse.wheel(0, 400);
      }
      await expect.poll(() => page.evaluate(() => scrollY)).toBeGreaterThan(0);
      const account = page.getByRole('button', { name: '账户菜单', exact: true });
      await account.focus();
      await account.press('Enter');
      const menu = page.locator('.account-menu .ant-dropdown');
      await expect(menu).toBeVisible();
      await expect(menu.getByRole('menuitem', { name: '退出登录' })).toBeInViewport();
      await page.keyboard.press('Escape');
      await expect(menu).toBeHidden();
      await expect(account).toBeFocused();
      await account.click();
      await expect(menu).toBeVisible();
      await page.getByRole('heading', { name: '系统设置', exact: true }).click();
      await expect(menu).toBeHidden();
      expect(backend.requests.every(({ method }) => method === 'GET')).toBe(true);
    });
  }
}
