// router.svelte.ts's module-level `$state` only compiles through Vite's
// Svelte pipeline, so `parse()` can't be unit-tested by importing it into a
// plain Node/Playwright-test context (there's no vitest/unit runner in this
// package either -- see package.json). Instead we drive it end-to-end
// through the browser: navigate to a hash and read back what the app
// actually parsed it into, via Placeholder.svelte's `.placeholder-route`
// element ({JSON.stringify(route)}). #/signin is exercised separately in
// app.spec.ts, since it renders SignIn.svelte, not Placeholder.svelte.
import { test, expect } from './fixtures';

async function routeFor(page: import('@playwright/test').Page, hash: string): Promise<unknown> {
  await page.goto(hash ? `/${hash}` : '/');
  const text = await page.locator('.placeholder-route').textContent();
  return JSON.parse(text ?? 'null');
}

test.describe('router: parse()', () => {
  test('no hash and "#/" both land on overview', async ({ page }) => {
    expect(await routeFor(page, '')).toEqual({ name: 'overview' });
    expect(await routeFor(page, '#/')).toEqual({ name: 'overview' });
  });

  test('an unrecognized top-level path falls back to overview', async ({ page }) => {
    expect(await routeFor(page, '#/nonsense')).toEqual({ name: 'overview' });
  });

  test('#/operator', async ({ page }) => {
    expect(await routeFor(page, '#/operator')).toEqual({ name: 'operator' });
  });

  test('#/t/acme is the tenant overview', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme')).toEqual({ name: 'tenant', slug: 'acme' });
  });

  test('#/t/acme/repos', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/repos')).toEqual({ name: 'repos', slug: 'acme' });
  });

  test('#/t/acme/repos/<owner>/<repo>', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/repos/kritik/kritik')).toEqual({
      name: 'repo',
      slug: 'acme',
      owner: 'kritik',
      repo: 'kritik',
    });
  });

  test('a malformed repos sub-path falls back to the tenant overview, not global', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/repos/only-owner')).toEqual({ name: 'tenant', slug: 'acme' });
  });

  test('#/t/acme/pulls/<owner>/<repo>/<n> parses the number as a JS number', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/pulls/kritik/kritik/42')).toEqual({
      name: 'pull',
      slug: 'acme',
      owner: 'kritik',
      repo: 'kritik',
      number: 42,
    });
  });

  test('a non-numeric pull number falls back to the tenant overview', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/pulls/kritik/kritik/abc')).toEqual({ name: 'tenant', slug: 'acme' });
  });

  test('#/t/acme/reviews/<id> with no tab', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/reviews/r1')).toEqual({ name: 'review', slug: 'acme', id: 'r1' });
  });

  test('#/t/acme/reviews/<id>/<tab> with a valid tab', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/reviews/r1/diff')).toEqual({
      name: 'review',
      slug: 'acme',
      id: 'r1',
      tab: 'diff',
    });
  });

  test('an unknown review tab is dropped, not rejected', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/reviews/r1/bogus')).toEqual({ name: 'review', slug: 'acme', id: 'r1' });
  });

  test('#/t/acme/admin with no section', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/admin')).toEqual({ name: 'admin', slug: 'acme' });
  });

  test('#/t/acme/admin/<section>', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/admin/tokens')).toEqual({
      name: 'admin',
      slug: 'acme',
      section: 'tokens',
    });
  });

  test('#/t/acme/queue, /usage and /followups', async ({ page }) => {
    expect(await routeFor(page, '#/t/acme/queue')).toEqual({ name: 'queue', slug: 'acme' });
    expect(await routeFor(page, '#/t/acme/usage')).toEqual({ name: 'usage', slug: 'acme' });
    expect(await routeFor(page, '#/t/acme/followups')).toEqual({ name: 'followups', slug: 'acme' });
  });
});
