import { test, expect, DEFAULT_ME } from './fixtures';

// The server mounts the UI, API and auth routes under KRITIK_WEB_URL's path.
// The preview server only serves the root, so the prefix is added here by
// proxying /kritik/<asset> to /<asset>.
test('served under a path prefix, every request stays under it', async ({ page }) => {
  const paths: string[] = [];
  page.on('request', (r) => paths.push(new URL(r.url()).pathname));
  await page.route(/\/kritik\/(index\.html)?$|\/kritik\/(assets\/|favicon)/, async (route) => {
    const u = new URL(route.request().url());
    u.pathname = u.pathname.replace(/^\/kritik/, '') || '/';
    await route.fulfill({ response: await route.fetch({ url: u.toString() }) });
  });
  await page.route('**/kritik/api/v1/me', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(DEFAULT_ME) }),
  );
  await page.goto('/kritik/index.html');
  await expect(page.locator('.tenant-switch option')).toHaveText(['acme']);
  expect(paths).toContain('/kritik/api/v1/me');
  expect(paths).toContain('/kritik/api/events');
  expect(paths.filter((p) => !p.startsWith('/kritik/'))).toEqual([]);
});
