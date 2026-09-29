import { test, expect, DEFAULT_ME, DEFAULT_PROVIDERS } from './fixtures';

test.describe('signed-out shell', () => {
  // The dashboard has no public content: a 401 from /api/v1/me on the bare
  // root -- same as on any other route -- shows the sign-in page rather
  // than a public overview shell.
  test('a 401 at the bare root bounces to sign-in with providers', async ({ page, mockProviders }) => {
    await mockProviders();
    await page.route('**/api/v1/me', (route) =>
      route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'unauthorized', message: 'no session' }),
      }),
    );
    await page.goto('/');
    await expect(page).toHaveURL(/#\/signin$/);
    await expect(page.locator('.signin-card h1')).toHaveText('kritik');

    const link = page.locator('.signin-provider');
    await expect(link).toHaveAttribute('href', /return_to=%23%2F$/);
  });

  test('a 401 from the API bounces to sign-in and remembers the return path', async ({ page, mockProviders }) => {
    await mockProviders();
    await page.route('**/api/v1/me', (route) =>
      route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'unauthorized', message: 'no session' }),
      }),
    );
    await page.goto('/#/a/github/acme/repos');
    await expect(page).toHaveURL(/#\/signin$/);
    await expect(page.locator('.signin-card h1')).toHaveText('kritik');

    const link = page.locator('.signin-provider');
    await expect(link).toHaveAttribute('href', /return_to=%23%2Fa%2Fgithub%2Facme%2Frepos/);
  });

  // "#/signin/" parses as the sign-in route, so a 401 there must neither
  // loop nor record the sign-in page itself as the place to return to.
  test('a 401 on #/signin/ stays on sign-in and returns to the overview', async ({ page, mockProviders }) => {
    await mockProviders();
    await page.route('**/api/v1/me', (route) =>
      route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({ code: 'unauthorized', message: 'no session' }),
      }),
    );
    await page.goto('/#/signin/');
    await expect(page.locator('.signin-card h1')).toHaveText('kritik');

    const link = page.locator('.signin-provider');
    await expect(link).toHaveAttribute('href', /return_to=%23%2F$/);
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

  test('the admin signs in with a password and returns to where they were', async ({ page, mockProviders }) => {
    await mockProviders([{ name: 'local', type: 'local', displayName: 'Admin' }, ...DEFAULT_PROVIDERS]);
    let signedIn = false;
    await page.route('**/api/v1/me', (route) =>
      signedIn
        ? route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ...DEFAULT_ME, admin: true }) })
        : route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ code: 'unauthenticated' }) }),
    );
    let sent: unknown;
    await page.route('**/auth/local', async (route) => {
      sent = route.request().postDataJSON();
      expect(route.request().headers()['x-kritik']).toBe('1');
      signedIn = true;
      await route.fulfill({ status: 204 });
    });
    await page.goto('/#/admin');
    await expect(page).toHaveURL(/#\/signin$/);
    const form = page.getByRole('form', { name: 'Admin sign-in' });
    await form.getByLabel('Username').fill('admin');
    await form.getByLabel('Password').fill('hunter2');
    await form.getByRole('button', { name: 'Sign in' }).click();
    await expect(page).toHaveURL(/#\/admin$/);
    expect(sent).toEqual({ user: 'admin', password: 'hunter2' });
  });

  test('a wrong password says so and clears the field', async ({ page, mockProviders }) => {
    await mockProviders([{ name: 'local', type: 'local', displayName: 'Admin' }]);
    await page.route('**/auth/local', (route) =>
      route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ code: 'invalid_credentials' }) }),
    );
    await page.goto('/#/signin');
    await page.getByLabel('Username').fill('admin');
    await page.getByLabel('Password').fill('wrong');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByRole('alert')).toHaveText('Wrong username or password.');
    await expect(page.getByLabel('Password')).toHaveValue('');
    await expect(page.locator('a.signin-provider')).toHaveCount(0);
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
  test('shows the account switcher, the section tabs and the user menu', async ({ page, signIn }) => {
    await signIn({ ...DEFAULT_ME, admin: true });
    await page.goto('/');

    await expect(page.locator('.account-button')).toHaveText('All accounts');
    await page.locator('.account-button').click();
    await expect(page.getByRole('navigation', { name: 'Accounts' }).getByRole('link')).toHaveText(['All accounts', 'github/acme']);
    await page.keyboard.press('Escape');
    await expect(page.getByRole('navigation', { name: 'Accounts' })).toBeHidden();

    // The sections are tabs under the topbar's first row, above the page.
    const tabs = page.locator('.topbar').getByRole('navigation', { name: 'Sections' }).getByRole('link');
    await expect(tabs).toHaveText(['Overview', 'Pull requests', 'Settings']);
    await expect(tabs.first()).toHaveAttribute('href', '#/a/github/acme');
    await expect(page.locator('.sections [aria-current]')).toHaveCount(0);
    const bar = await page.locator('.topbar').boundingBox();
    const main = await page.locator('main.page').boundingBox();
    expect(bar && main && bar.y + bar.height <= main.y).toBe(true);
    await expect(page.locator('.user-menu summary')).toHaveAttribute('title', DEFAULT_ME.user.displayName);

    await page.locator('.user-menu summary').click();
    await expect(page.locator('.user-name')).toHaveText(DEFAULT_ME.user.displayName);
    await expect(page.locator('.user-email')).toHaveText(DEFAULT_ME.user.email);
  });

  test("settings lists a member only the account's repositories", async ({ page, signIn }) => {
    await signIn(DEFAULT_ME);
    await page.goto('/#/a/github/acme/repos');
    const nav = page.getByRole('navigation', { name: 'Settings' });
    await expect(nav.getByRole('link')).toHaveText(['Repositories']);
    await expect(page.locator('.sections .section-tab.active')).toHaveText('Settings');
  });

  test("settings lists an admin the account's configuration and the admin console", async ({ page, signIn }) => {
    await signIn({ ...DEFAULT_ME, admin: true });
    await page.goto('/#/a/github/acme/repos');
    const nav = page.getByRole('navigation', { name: 'Settings' });
    await expect(nav.getByRole('link')).toHaveText(['Repositories', 'Configuration', 'Audit log', 'Admin console']);
    await nav.getByRole('link', { name: 'Admin console' }).click();
    await expect(page).toHaveURL(/#\/admin$/);
    await expect(nav.getByRole('link', { name: 'Admin console' })).toHaveAttribute('aria-current', 'page');
    await expect(page.locator('.sections .section-tab.active')).toHaveText('Settings');
  });

  test('switching accounts in the menu navigates to that account', async ({ page, signIn }) => {
    await signIn({
      ...DEFAULT_ME,
      accounts: ['github/acme', 'github/globex'],
    });
    await page.goto('/#/a/github/acme/pulls');
    await expect(page.locator('.account-button')).toHaveText('github/acme');
    await page.locator('.account-button').click();
    await page.getByRole('navigation', { name: 'Accounts' }).getByRole('link', { name: 'github/globex' }).click();
    await expect(page).toHaveURL(/#\/a\/github\/globex$/);
    await expect(page.getByRole('navigation', { name: 'Accounts' })).toBeHidden();
    await expect(page.locator('.account-button')).toHaveText('github/globex');
  });

  test('a signed-in visit to #/signin redirects to the overview', async ({ page, signIn, mockProviders }) => {
    await signIn();
    await mockProviders();
    await page.goto('/#/signin');
    await expect(page).toHaveURL(/#\/$/);
    await expect(page.locator('.signin-card')).toHaveCount(0);
    await expect(page.locator('.account-button')).toBeVisible();
  });

  test('signing out clears the shell and returns to sign-in', async ({ page, signIn }) => {
    await signIn();
    await page.route('**/auth/logout', (route) => route.fulfill({ status: 204 }));
    await page.goto('/');

    await page.locator('.user-menu summary').click();
    await page.getByRole('button', { name: 'Sign out' }).click();

    await expect(page).toHaveURL(/#\/signin$/);
    await expect(page.locator('.account-button')).toHaveCount(0);
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

  // The bundle runs after the first paint may have happened, so the class
  // must not depend on it.
  test('applies the OS or stored theme without the bundle', async ({ page }) => {
    await page.route('**/assets/*.js', (route) => route.abort());
    const currentClass = () => page.evaluate(() => document.documentElement.className);
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.goto('/');
    expect(await currentClass()).toBe('dark');

    await page.addInitScript(() => localStorage.setItem('kritik-theme', 'light'));
    await page.reload();
    expect(await currentClass()).toBe('light');
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

  test('Ctrl/Cmd+K opens the command palette; typing filters; Enter navigates', async ({ page, signIn }) => {
    await signIn({ ...DEFAULT_ME, admin: true });
    await page.goto('/');
    await page.keyboard.press('ControlOrMeta+k');
    await expect(page.locator('.palette-input input')).toBeFocused();

    await page.keyboard.type('console');
    // The console's sections match too, after the console itself.
    await expect(page.locator('.row-title').first()).toHaveText('Admin console');

    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/#\/admin$/);
    await expect(page.locator('.palette-overlay')).toHaveCount(0);
  });

  test('the palette shows an empty state when nothing matches', async ({ page }) => {
    await page.goto('/');
    await page.keyboard.press('ControlOrMeta+k');
    await page.keyboard.type('xyz-nothing-matches');
    await expect(page.locator('.palette-empty')).toBeVisible();
  });
});

test('a stream the server refuses for a dead session sends the tab to sign-in', async ({ page, mockProviders }) => {
  await mockProviders();
  let meCalls = 0;
  await page.route('**/api/v1/me', (route) => {
    meCalls++;
    return meCalls === 1
      ? route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(DEFAULT_ME) })
      : route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ code: 'unauthenticated', message: 'no session' }) });
  });
  let streams = 0;
  await page.route('**/api/events', (route) => {
    streams++;
    return route.fulfill({ status: 401, contentType: 'application/json', body: '{"code":"unauthenticated"}' });
  });
  await page.goto('/#/a/github/acme/repos');
  await expect(page).toHaveURL(/#\/signin$/, { timeout: 10_000 });
  await expect(page.locator('.signin-provider')).toHaveAttribute('href', /return_to=%23%2Fa%2Fgithub%2Facme%2Frepos/);
  const after = streams;
  await page.waitForTimeout(2_500);
  expect(streams).toBe(after);
});

test('the topbar says when live updates are down, and when they are back', async ({ page, signIn }) => {
  await signIn();
  let up = false;
  await page.route('**/api/events', (route) =>
    up ? route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' }) : route.fulfill({ status: 503, body: '' }),
  );
  await page.goto('/');
  const live = page.locator('.topbar .live');
  await expect(live).toHaveText('Live updates on');
  await expect(live).toHaveText('Reconnecting…');
  await expect(live).toHaveAttribute('title', /^Live updates stopped at .+; this page may be out of date\.$/);

  up = true;
  // The next attempt waits out the reconnect backoff, a few seconds by now.
  await expect(live).toHaveText('Live updates on', { timeout: 10_000 });
});

test('a 401 from a page while signed in stays on sign-in', async ({ page, signIn, mockProviders }) => {
  await signIn();
  await mockProviders();
  await page.route('**/api/v1/accounts/github/acme/repos**', (route) =>
    route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ code: 'unauthenticated', message: 'no session' }) }),
  );
  await page.goto('/#/a/github/acme/repos');
  await expect(page).toHaveURL(/#\/signin$/);
  await page.waitForTimeout(500);
  await expect(page).toHaveURL(/#\/signin$/);
  await expect(page.locator('.account-button')).toHaveCount(0);
});
