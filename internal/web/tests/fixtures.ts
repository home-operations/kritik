// Shared Playwright fixtures for the dashboard's E2E suite. Every test runs
// against the static preview build (see playwright.config.ts's webServer),
// not a live Go backend, so any endpoint a test cares about must be mocked
// via page.route -- otherwise Vite preview's SPA fallback serves index.html
// (200, text/html) for it, which is rarely what the page under test expects.
import { test as base, expect } from '@playwright/test';
import type { Me, SignInProvider } from '../src/lib/types';

export const DEFAULT_ME: Me = {
  account: { id: 'u1', displayName: 'Ada Lovelace', email: 'ada@example.com', avatarUrl: '' },
  operator: false,
  tenants: [{ slug: 'acme', role: 'admin', managedBy: 'file' }],
};

export const DEFAULT_PROVIDERS: SignInProvider[] = [{ name: 'github', type: 'github', displayName: 'GitHub' }];

interface Fixtures {
  signIn: (me?: Me) => Promise<void>;
  mockProviders: (providers?: SignInProvider[]) => Promise<void>;
}

export const test = base.extend<Fixtures>({
  // Auto-mock /api/events for every test: events.svelte.ts opens a real
  // EventSource against it, and against an unmocked preview server that's an
  // HTML document, not an event stream, which just churns the reconnect
  // backoff in the background for the life of the test.
  page: async ({ page }, use) => {
    await page.route('**/api/events', (route) =>
      route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }),
    );
    await use(page);
  },

  signIn: async ({ page }, use) => {
    await use(async (me = DEFAULT_ME) => {
      await page.route('**/api/v1/me', (route) =>
        route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(me) }),
      );
    });
  },

  mockProviders: async ({ page }, use) => {
    await use(async (providers = DEFAULT_PROVIDERS) => {
      await page.route('**/auth/providers', (route) =>
        route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(providers) }),
      );
    });
  },
});

export { expect };
