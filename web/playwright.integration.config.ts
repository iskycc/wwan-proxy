import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  testMatch: '**/integration.spec.ts',
  outputDir: './test-results-integration',
  workers: 1,
  timeout: 60000,
  expect: { timeout: 10000 },
  use: {
    baseURL: 'http://127.0.0.1:4180',
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
      : {},
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: 'node tests/go-server.mjs',
    url: 'http://127.0.0.1:4180/api/health',
    timeout: 120000,
    reuseExistingServer: false,
  },
});
