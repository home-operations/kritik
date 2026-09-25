import { test, expect, DEFAULT_ME } from './fixtures';

test.describe('signed-out shell', () => {
  test('lands on the overview placeholder with no tenant chrome', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('.placeholder h1')).toHaveText('Overview');
    await expect(page.locator('.wordmark')).toHaveText('kritik');
    await expect(page.locator('.tenant-switch')).toHaveCount(0);
    await expect(page.locator('.nav')).toHaveCount(0);
    await expect(page.locator('.account-menu')).toHaveCount(0);
    // palette, help, theme -- no operator link (no `me`), no account menu.
    await expect(page.locator('.actions .btn-icon')).toHaveCount(3);
  });

  test('a 401 from the API bounces to sign-in and remembers the return path', async ({ page }) => {
    await page.route('**/api/v1/me', (route) =>
      route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'unauthorized', message: 'no session' }),
      }),
    );
    await page.goto('/#/t/acme/repos');
    await expect(page).toHaveURL(/#\/signin$/);
    await expect(page.locator('.signin-card h1')).toHaveText('kritik');
  });
});

test.describe('sign-in page', () => {
  test('lists providers with a login link carrying the return path', async ({ page, mockProviders }) => {
    await mockProviders();
    await page.goto('/#/signin');
    const link = page.locator('.signin-provider');
    await expect(link).toContainText('GitHub');
    await expect(link).toHaveAttribute('href', /\/auth\/login\/github\?return_to=/);
  });

  test('shows an empty state when no providers are configured', async ({ page, mockProviders }) => {
    await mockProviders([]);
    await page.goto('/#/signin');
    await expect(page.locator('.signin-empty')).toHaveText('No sign-in providers configured.');
  });

  test('shows an error state when the providers request fails', async ({ page }) => {
    await page.route('**/auth/providers', (route) =>
      route.fulfill({ status: 500, contentType: 'application/json', body: '{}' }),
    );
    await page.goto('/#/signin');
    await expect(page.locator('.signin-error')).toBeVisible();
  });
});

test.describe('signed-in shell', () => {
  test('shows tenant nav, the admin link, and the account menu for an admin', async ({ page, signIn }) => {
    await signIn();
    await page.goto('/');

    await expect(page.locator('.tenant-switch option')).toHaveText(['acme']);
    await expect(page.locator('.nav a')).toHaveCount(7); // Overview/Repos/Pulls/Queue/Usage/Follow-ups/Admin
    await expect(page.locator('.account-menu summary')).toHaveAttribute('title', DEFAULT_ME.account.displayName);

    await page.locator('.account-menu summary').click();
    await expect(page.locator('.account-name')).toHaveText(DEFAULT_ME.account.displayName);
    await expect(page.locator('.account-email')).toHaveText(DEFAULT_ME.account.email);
  });

  test('hides the admin link for a non-admin member', async ({ page, signIn }) => {
    await signIn({ ...DEFAULT_ME, tenants: [{ slug: 'acme', role: 'member', managedBy: 'file' }] });
    await page.goto('/');
    await expect(page.locator('.nav a')).toHaveCount(6);
  });

  test('shows the operator console link for an operator account', async ({ page, signIn }) => {
    await signIn({ ...DEFAULT_ME, operator: true });
    await page.goto('/');
    await expect(page.locator('.actions a[title="Operator console"]')).toBeVisible();
  });

  test('switching tenants in the dropdown navigates to that tenant', async ({ page, signIn }) => {
    await signIn({
      ...DEFAULT_ME,
      tenants: [
        { slug: 'acme', role: 'admin', managedBy: 'file' },
        { slug: 'globex', role: 'member', managedBy: 'file' },
      ],
    });
    await page.goto('/');
    await page.locator('.tenant-switch').selectOption('globex');
    await expect(page).toHaveURL(/#\/t\/globex$/);
  });

  test('signing out clears the shell and returns to sign-in', async ({ page, signIn }) => {
    await signIn();
    await page.route('**/auth/logout', (route) => route.fulfill({ status: 204 }));
    await page.goto('/');

    await page.locator('.account-menu summary').click();
    await page.getByRole('button', { name: 'Sign out' }).click();

    await expect(page).toHaveURL(/#\/signin$/);
    await expect(page.locator('.tenant-switch')).toHaveCount(0);
  });
});

test.describe('theme toggle', () => {
  test('cycles auto -> light -> dark -> auto and persists the choice', async ({ page }) => {
    await page.goto('/');
    const button = page.locator('.actions button[title^="Theme:"]');
    const currentClass = () => page.evaluate(() => document.documentElement.className);
    const stored = () => page.evaluate(() => localStorage.getItem('kritik-theme'));

    // auto, resolved against a light-scheme test environment
    await expect.poll(currentClass).toBe('light');

    await button.click();
    await expect.poll(stored).toBe('light');
    await expect.poll(currentClass).toBe('light');

    await button.click();
    await expect.poll(stored).toBe('dark');
    await expect.poll(currentClass).toBe('dark');

    await button.click();
    await expect.poll(stored).toBe('auto');
  });
});

test.describe('keyboard shortcuts', () => {
  test('"?" opens the help overlay; Escape closes it', async ({ page }) => {
    await page.goto('/');
    await page.keyboard.press('?');
    await expect(page.locator('.help-card h2')).toHaveText('Keyboard shortcuts');
    await page.keyboard.press('Escape');
    await expect(page.locator('.help-overlay')).toHaveCount(0);
  });

  test('Ctrl/Cmd+K opens the command palette; typing filters; Enter navigates', async ({ page }) => {
    await page.goto('/');
    await page.keyboard.press('ControlOrMeta+k');
    await expect(page.locator('.palette-input input')).toBeFocused();

    await page.keyboard.type('operator');
    await expect(page.locator('.palette-row')).toHaveCount(1);
    await expect(page.locator('.row-title')).toHaveText('Operator console');

    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/#\/operator$/);
    await expect(page.locator('.palette-overlay')).toHaveCount(0);
  });

  test('the palette shows an empty state when nothing matches', async ({ page }) => {
    await page.goto('/');
    await page.keyboard.press('ControlOrMeta+k');
    await page.keyboard.type('xyz-nothing-matches');
    await expect(page.locator('.palette-empty')).toBeVisible();
  });
});
