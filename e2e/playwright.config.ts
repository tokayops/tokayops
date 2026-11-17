import fs from 'fs';
import os from 'os';
import path from 'path';
import { defineConfig, devices } from '@playwright/test';

// macOS 27 TCC-protects ~/Library/Application Support/Firefox for the real
// Firefox. The bundled build looks there before it honors -profile, gets
// denied without a prompt, and then hangs in headless startup. Firefox 158
// no longer does that lookup; Playwright 1.58 still ships 146.
function firefoxEnv(): NodeJS.ProcessEnv | undefined {
  if (process.platform !== 'darwin') return undefined;
  const root = path.join(os.tmpdir(), 'tokay-playwright-firefox');
  const appData = path.join(root, 'app');
  const localData = path.join(root, 'local');
  fs.mkdirSync(appData, { recursive: true });
  fs.mkdirSync(localData, { recursive: true });
  return {
    ...process.env,
    MOZ_APP_DATA: appData,
    MOZ_LOCAL_APP_DATA: localData,
  };
}

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : 8,
  reporter: [
    ['html', { outputFolder: 'playwright-report', open: 'never' }],
    ['list']
  ],
  use: {
    baseURL: process.env.BASE_URL || 'http://localhost:8081',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'on-first-retry',
  },
  projects: [
    {
      name: 'setup',
      testMatch: /global\.setup\.ts/,
    },
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        storageState: '.auth/user.json',
      },
      dependencies: ['setup'],
    },
    {
      name: 'firefox',
      use: {
        ...devices['Desktop Firefox'],
        storageState: '.auth/user.json',
        launchOptions: { env: firefoxEnv() },
      },
      dependencies: ['setup'],
    },
  ],
  outputDir: 'test-results/',
});
