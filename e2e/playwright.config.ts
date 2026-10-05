import { defineConfig } from '@playwright/test';

// One browser, one worker, in order. The tests share one workshop built from
// an empty database by the first of them, the way a morning builds on itself;
// running them in parallel would race over it.
export default defineConfig({
  testDir: './tests',
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 60_000,
  reporter: [['list']],
  use: {
    baseURL: process.env.FREESMS_URL ?? 'http://localhost:8080',
    trace: 'retain-on-failure',
  },
});
