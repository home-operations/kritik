// router.svelte.ts's module-level `$state` only compiles through Vite's
// Svelte pipeline, so `parse()` can't be unit-tested by importing it into a
// plain Node/Playwright-test context. Instead we drive it end-to-end through
// the browser: navigate to a hash and read back what the app actually parsed
// it into, via Placeholder.svelte's `.placeholder-route` element
// ({JSON.stringify(route)}). #/signin is exercised separately in
// app.spec.ts, since it renders SignIn.svelte, not Placeholder.svelte.
//
// Pure parse()/href() round-trip and malformed-hash coverage lives in
// routes.spec.ts, which imports those functions directly with no browser.
import { test, expect } from './fixtures';
import type { Route } from '../src/lib/routes';

async function expectRoute(page: import('@playwright/test').Page, hash: string, route: Route): Promise<void> {
  await page.goto(hash ? `/${hash}` : '/');
  await expect(page.locator('.placeholder-route')).toHaveText(JSON.stringify(route));
}

test.describe('router: parse()', () => {
  test('no hash and "#/" both land on overview', async ({ page }) => {
    await expectRoute(page, '', { name: 'overview' });
    await expectRoute(page, '#/', { name: 'overview' });
  });

  test('an unrecognized top-level path falls back to overview', async ({ page }) => {
    await expectRoute(page, '#/nonsense', { name: 'overview' });
  });

  test('#/operator', async ({ page }) => {
    await expectRoute(page, '#/operator', { name: 'operator' });
  });

  test('#/t/acme is the tenant overview', async ({ page }) => {
    await expectRoute(page, '#/t/acme', { name: 'tenant', slug: 'acme' });
  });

  test('#/t/acme/repos', async ({ page }) => {
    await expectRoute(page, '#/t/acme/repos', { name: 'repos', slug: 'acme' });
  });

  test('#/t/acme/repos/<owner>/<repo>', async ({ page }) => {
    await expectRoute(page, '#/t/acme/repos/kritik/kritik', {
      name: 'repo',
      slug: 'acme',
      owner: 'kritik',
      repo: 'kritik',
    });
  });

  test('a malformed repos sub-path falls back to the tenant overview, not global', async ({ page }) => {
    await expectRoute(page, '#/t/acme/repos/only-owner', { name: 'tenant', slug: 'acme' });
  });

  test('#/t/acme/pulls/<owner>/<repo>/<n> parses the number as a JS number', async ({ page }) => {
    await expectRoute(page, '#/t/acme/pulls/kritik/kritik/42', {
      name: 'pull',
      slug: 'acme',
      owner: 'kritik',
      repo: 'kritik',
      number: 42,
    });
  });

  test('a non-numeric pull number falls back to the tenant overview', async ({ page }) => {
    await expectRoute(page, '#/t/acme/pulls/kritik/kritik/abc', { name: 'tenant', slug: 'acme' });
  });

  test('#/t/acme/reviews/<id> with no tab', async ({ page }) => {
    await expectRoute(page, '#/t/acme/reviews/r1', { name: 'review', slug: 'acme', id: 'r1' });
  });

  test('#/t/acme/reviews/<id>/<tab> with a valid tab', async ({ page }) => {
    await expectRoute(page, '#/t/acme/reviews/r1/diff', {
      name: 'review',
      slug: 'acme',
      id: 'r1',
      tab: 'diff',
    });
  });

  test('an unknown review tab is dropped, not rejected', async ({ page }) => {
    await expectRoute(page, '#/t/acme/reviews/r1/bogus', { name: 'review', slug: 'acme', id: 'r1' });
  });

  test('#/t/acme/admin with no section', async ({ page }) => {
    await expectRoute(page, '#/t/acme/admin', { name: 'admin', slug: 'acme' });
  });

  test('#/t/acme/admin/<section>', async ({ page }) => {
    await expectRoute(page, '#/t/acme/admin/tokens', {
      name: 'admin',
      slug: 'acme',
      section: 'tokens',
    });
  });

  test('#/t/acme/queue, /usage and /followups', async ({ page }) => {
    await expectRoute(page, '#/t/acme/queue', { name: 'queue', slug: 'acme' });
    await expectRoute(page, '#/t/acme/usage', { name: 'usage', slug: 'acme' });
    await expectRoute(page, '#/t/acme/followups', { name: 'followups', slug: 'acme' });
  });
});
