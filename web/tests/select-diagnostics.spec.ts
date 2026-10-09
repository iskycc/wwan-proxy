import { readFileSync } from 'node:fs';
import { test, expect } from './fixtures';

const script = readFileSync(
  new URL('../../scripts/debug-webui-select.js', import.meta.url),
  'utf8',
);
type Report = {
  environment: {
    viewport: { width: number; height: number };
    media: Record<string, boolean>;
  };
  events: { type: string; trusted: boolean }[];
  snapshots: {
    viewport: { width: number; height: number };
    media: Record<string, boolean>;
    expanded: string;
    popups: { inViewport: boolean; computed: { opacity: string } }[];
  }[];
  errors: unknown[];
};

test('诊断等待真实打开操作，单纯聚焦不会提前完成采集', async ({ page, backend }) => {
  await page.goto('/');
  const input = page.getByRole('combobox', { name: '界面主题', exact: true });
  await expect(input).toBeVisible();
  const reports: Report[] = [];
  page.on('console', (message) => {
    if (message.text().startsWith('WWAN_SELECT_DIAGNOSTIC\n')) {
      reports.push(JSON.parse(message.text().split('\n').slice(1).join('\n')) as Report);
    }
  });
  await page.evaluate(script);
  await input.focus();
  await page.waitForTimeout(2200);
  expect(reports).toEqual([]);
  await input.press('ArrowDown');
  await expect.poll(() => reports.length).toBe(1);
  expect(reports[0].events[0]).toMatchObject({ type: 'keydown', trusted: true });
  expect(reports[0].snapshots.at(-1)).toMatchObject({ expanded: 'true' });
  expect(reports[0].snapshots.at(-1)!.popups[0].inViewport).toBe(true);
  expect(backend.requests.every(({ method }) => method === 'GET')).toBe(true);
});

test('诊断使用交互时的视口和动画偏好，记录采集中的环境变化', async ({ page, backend }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ reducedMotion: 'no-preference' });
  await page.goto('/');
  const input = page.getByRole('combobox', { name: '界面主题', exact: true });
  await expect(input).toBeVisible();
  await page.evaluate(script);
  await page.setViewportSize({ width: 1100, height: 900 });
  await page.emulateMedia({ reducedMotion: 'reduce' });
  const output = page.waitForEvent('console', {
    predicate: (message) => message.text().startsWith('WWAN_SELECT_DIAGNOSTIC\n'),
  });
  await input.press('ArrowDown');
  // Change preferences while the menu is open, before the remaining snapshots.
  await page.emulateMedia({ reducedMotion: 'no-preference' });
  const text = (await output).text();
  const report = JSON.parse(text.slice(text.indexOf('\n') + 1)) as Report;
  expect(report.environment.viewport).toMatchObject({ width: 1100, height: 900 });
  expect(report.environment.media['prefers-reduced-motion: reduce']).toBe(false);
  expect(report.snapshots[0].viewport).toMatchObject({ width: 1100, height: 900 });
  expect(report.snapshots[0].media['prefers-reduced-motion: reduce']).toBe(true);
  expect(report.snapshots.at(-1)!.media['prefers-reduced-motion: reduce']).toBe(false);
  expect(report.snapshots.at(-1)!.popups[0].inViewport).toBe(true);
  expect(backend.requests.every(({ method }) => method === 'GET')).toBe(true);
});

for (const [name, style] of [
  ['正常菜单', ''],
  ['菜单移出视口', '.ant-select-dropdown { left: -1000vw !important; top: -1000vh !important; }'],
  ['菜单完全透明', '.ant-select-dropdown { opacity: 0 !important; }'],
]) {
  test(`现场诊断区分${name}，不更改选择或暴露配置`, async ({ page, backend }) => {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto('/');
    const input = page.getByRole('combobox', { name: '界面主题', exact: true });
    await expect(input).toBeVisible();
    if (style) await page.addStyleTag({ content: style });
    await page.evaluate(script);
    const output = page.waitForEvent('console', {
      predicate: (message) => message.text().startsWith('WWAN_SELECT_DIAGNOSTIC\n'),
    });
    const rect = await page.locator('.ant-select').filter({ has: input }).boundingBox();
    await page.mouse.click(rect!.x + rect!.width - 16, rect!.y + rect!.height / 2);
    const text = (await output).text();
    const report = JSON.parse(text.slice(text.indexOf('\n') + 1)) as Report;
    expect(report.errors).toEqual([]);
    expect(report.events).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ type: 'mousedown', trusted: true }),
        expect.objectContaining({ type: 'click', trusted: true }),
      ]),
    );
    expect(report.snapshots.some(({ expanded }) => expanded === 'true')).toBe(true);
    const popup = report.snapshots.at(-1)!.popups[0];
    expect(popup.inViewport).toBe(name !== '菜单移出视口');
    expect(popup.computed.opacity).toBe(name === '菜单完全透明' ? '0' : '1');
    await expect(input).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('html')).toHaveAttribute('data-preference', 'system');
    expect(text).not.toContain(backend.auth.username!);
    expect(text).not.toContain(backend.overview.servers[0].name);
    expect(backend.requests.every(({ method }) => method === 'GET')).toBe(true);
  });
}
