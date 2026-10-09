import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  testIgnore: process.env.WWAN_UI_SCREENSHOTS
    ? '**/integration.spec.ts'
    : ['**/integration.spec.ts', '**/screenshots.spec.ts'],
  fullyParallel: true,
  workers: 2,
  timeout: 30000,
  expect: { timeout: 7000 },
  use: {
    actionTimeout: 10000,
    baseURL: 'http://127.0.0.1:4178',
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
      : {},
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: 'node tests/server.mjs',
    url: 'http://127.0.0.1:4178',
    reuseExistingServer: false,
  },
});
