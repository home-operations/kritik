import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? 'html' : 'list',
  use: {
    baseURL: 'http://127.0.0.1:4173',
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    // --host 127.0.0.1 pins the preview server to IPv4 loopback: with no
    // --host, vite preview binds the bare string "localhost", which Node can
    // resolve IPv6-first (::1), so a v4-only 127.0.0.1 client (Playwright's
    // own webServer readiness probe, and baseURL above) would otherwise time
    // out with connection refused despite the server being up. --strictPort
    // fails fast instead of silently picking another port if 4173 is busy.
    command: 'npm run build && npm run preview -- --port 4173 --host 127.0.0.1 --strictPort',
    url: 'http://127.0.0.1:4173',
    reuseExistingServer: !process.env.CI,
  },
});
