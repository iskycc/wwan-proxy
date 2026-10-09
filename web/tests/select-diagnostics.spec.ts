import { readFileSync } from 'node:fs';
import { test, expect } from './fixtures';

const script = readFileSync(
  new URL('../../scripts/debug-webui-select.js', import.meta.url),
  'utf8',
);
type Report = {
  events: { type: string; trusted: boolean }[];
  snapshots: {
    expanded: string;
    popups: { inViewport: boolean; computed: { opacity: string } }[];
  }[];
  errors: unknown[];
};

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
