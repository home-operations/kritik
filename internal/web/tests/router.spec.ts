// router.svelte.ts's module-level `$state` only compiles through Vite's
// Svelte pipeline, so `parse()` can't be unit-tested by importing it into a
// plain Node/Playwright-test context. Instead we drive it end-to-end through
// the browser: navigate to a hash and read back what the app actually parsed
// it into, via the page host's `data-route` attribute
// ({JSON.stringify(route)}, see pages/Page.svelte). #/signin is exercised
// separately in app.spec.ts, since it renders SignIn.svelte instead.
//
// Pure parse()/href() round-trip and malformed-hash coverage lives in
// routes.spec.ts, which imports those functions directly with no browser.
import { test, expect } from './fixtures';
import type { Route } from '../src/lib/routes';

async function expectRoute(page: import('@playwright/test').Page, hash: string, route: Route): Promise<void> {
  await page.goto(hash ? `/${hash}` : '/');
  await expect(page.locator('.route-host')).toHaveAttribute('data-route', JSON.stringify(route));
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

  test('#/a/acme is the account overview', async ({ page }) => {
    await expectRoute(page, '#/a/acme', { name: 'account', slug: 'acme' });
  });

  test('#/a/acme/repos', async ({ page }) => {
    await expectRoute(page, '#/a/acme/repos', { name: 'repos', slug: 'acme' });
  });

  test('#/a/acme/repos/<owner>/<repo>', async ({ page }) => {
    await expectRoute(page, '#/a/acme/repos/kritik/kritik', {
      name: 'repo',
      slug: 'acme',
      owner: 'kritik',
      repo: 'kritik',
    });
  });

  test('a malformed repos sub-path falls back to the account overview, not global', async ({ page }) => {
    await expectRoute(page, '#/a/acme/repos/only-owner', { name: 'account', slug: 'acme' });
  });

  test('#/a/acme/pulls/<owner>/<repo>/<n> parses the number as a JS number', async ({ page }) => {
    await expectRoute(page, '#/a/acme/pulls/kritik/kritik/42', {
      name: 'pull',
      slug: 'acme',
      owner: 'kritik',
      repo: 'kritik',
      number: 42,
    });
  });

  test('a non-numeric pull number falls back to the account overview', async ({ page }) => {
    await expectRoute(page, '#/a/acme/pulls/kritik/kritik/abc', { name: 'account', slug: 'acme' });
  });

  test('#/a/acme/reviews/<id> with no tab', async ({ page }) => {
    await expectRoute(page, '#/a/acme/reviews/r1', { name: 'review', slug: 'acme', id: 'r1' });
  });

  test('#/a/acme/reviews/<id>/<tab> with a valid tab', async ({ page }) => {
    await expectRoute(page, '#/a/acme/reviews/r1/diff', {
      name: 'review',
      slug: 'acme',
      id: 'r1',
      tab: 'diff',
    });
  });

  test('an unknown review tab is dropped, not rejected', async ({ page }) => {
    await expectRoute(page, '#/a/acme/reviews/r1/bogus', { name: 'review', slug: 'acme', id: 'r1' });
  });

  test('#/a/acme/admin with no section', async ({ page }) => {
    await expectRoute(page, '#/a/acme/admin', { name: 'admin', slug: 'acme' });
  });

  test('#/a/acme/admin/<section>', async ({ page }) => {
    await expectRoute(page, '#/a/acme/admin/tokens', {
      name: 'admin',
      slug: 'acme',
      section: 'tokens',
    });
  });

  test('#/a/acme/queue, /usage and /followups', async ({ page }) => {
    await expectRoute(page, '#/a/acme/queue', { name: 'queue', slug: 'acme' });
    await expectRoute(page, '#/a/acme/usage', { name: 'usage', slug: 'acme' });
    await expectRoute(page, '#/a/acme/followups', { name: 'followups', slug: 'acme' });
  });
});
