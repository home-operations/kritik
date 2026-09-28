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

  test('#/a/github/acme is the account overview', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme', { name: 'account', slug: 'github/acme' });
  });

  test('#/a/github/acme/repos', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/repos', { name: 'repos', slug: 'github/acme' });
  });

  test('#/a/github/acme/repos/<owner>/<repo>', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/repos/kritik/kritik', {
      name: 'repo',
      slug: 'github/acme',
      owner: 'kritik',
      repo: 'kritik',
    });
  });

  test('a malformed repos sub-path falls back to the account overview, not global', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/repos/only-owner', { name: 'account', slug: 'github/acme' });
  });

  test('#/a/github/acme/pulls/<owner>/<repo>/<n> parses the number as a JS number', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/pulls/kritik/kritik/42', {
      name: 'pull',
      slug: 'github/acme',
      owner: 'kritik',
      repo: 'kritik',
      number: 42,
    });
  });

  test('a non-numeric pull number falls back to the account overview', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/pulls/kritik/kritik/abc', { name: 'account', slug: 'github/acme' });
  });

  test('#/a/github/acme/reviews/<id> with no tab', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/reviews/r1', { name: 'review', slug: 'github/acme', id: 'r1' });
  });

  test('#/a/github/acme/reviews/<id>/<tab> with a valid tab', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/reviews/r1/diff', {
      name: 'review',
      slug: 'github/acme',
      id: 'r1',
      tab: 'diff',
    });
  });

  test('an unknown review tab is dropped, not rejected', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/reviews/r1/bogus', { name: 'review', slug: 'github/acme', id: 'r1' });
  });

  test('#/a/github/acme/admin with no section', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/admin', { name: 'admin', slug: 'github/acme' });
  });

  test('#/a/github/acme/admin/<section>', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/admin/tokens', {
      name: 'admin',
      slug: 'github/acme',
      section: 'tokens',
    });
  });

  test('#/a/github/acme/queue, /usage and /followups', async ({ page }) => {
    await expectRoute(page, '#/a/github/acme/queue', { name: 'queue', slug: 'github/acme' });
    await expectRoute(page, '#/a/github/acme/usage', { name: 'usage', slug: 'github/acme' });
    await expectRoute(page, '#/a/github/acme/followups', { name: 'followups', slug: 'github/acme' });
  });
});
