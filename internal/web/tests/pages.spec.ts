import { test, expect } from './fixtures';
import * as g from './golden';
import type { AccountDetail, Pull, ReviewStatus } from '../src/lib/types';

const T = `#/a/${g.SLUG}`;

test.beforeEach(async ({ page }) => {
  await g.mockApi(page, g.defaultApi());
});

test.describe('overview', () => {
  test('a single-account member stays on the breakdown instead of leaving for the account', async ({ page }) => {
    await page.goto('/#/');
    await expect(page.locator('.page-head h1')).toHaveText('All accounts');
    await expect(page).toHaveURL(/#\/$/);
    const rows = page.locator('table.account-breakdown tbody tr');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText(g.accountSummary.slug);
    await rows.first().getByRole('link', { name: g.accountSummary.slug }).click();
    await expect(page).toHaveURL(new RegExp(`#/a/${g.accountSummary.slug}$`));
  });

  test('several accounts each get a row, and the tiles add them up', async ({ page }) => {
    await g.mockApi(page, [[/\/api\/v1\/accounts$/, [g.accountSummary, { ...g.accountSummary, slug: 'beta' }]], ...g.defaultApi()]);
    await page.goto('/#/');
    await expect(page.locator('table.account-breakdown tbody tr')).toHaveCount(2);
    const tiles = page.getByRole('region', { name: 'Across all accounts' });
    await expect(tiles.locator('.tile').filter({ hasText: 'Reviews, last 7 days' })).toContainText(String(2 * g.accountSummary.reviews7d));
    await expect(tiles.locator('.tile').filter({ hasText: 'Repositories' })).toContainText(String(2 * g.accountSummary.repositories));
    await expect(tiles.locator('.tile').filter({ hasText: 'Spend this month' })).toContainText('$3.00');
  });
});

test('account overview shows tiles, recent reviews, queue and repositories', async ({ page }) => {
  await page.goto(`/${T}`);
  await expect(page.locator('.tile').first()).toContainText(String(g.accountSummary.reviews7d));
  await expect(page.locator('.tiles')).toContainText('$1.50');
  await expect(page.getByRole('meter', { name: 'Monthly tokens used' })).toHaveAttribute('aria-valuemax', String(g.accountSummary.usage.tokensPerMonth));
  await expect(page.locator('#ov-recent').locator('..').locator('..')).toContainText(g.pull.title);
  await expect(page.locator('.chips')).toContainText(`1 ${g.job.state}`);
  await expect(page.getByRole('region', { name: 'Repositories', exact: true }).locator('table.data')).toContainText(g.repoPage.items[0]!.fullName);
});

test('account overview links the open pulls whose last review failed or was capped', async ({ page }) => {
  const as = (n: number, status: ReviewStatus): Pull => ({ ...g.pull, number: n, url: g.pull.url.replace(/\d+$/, String(n)), lastReview: { ...g.pull.lastReview!, status } });
  const attention = page.getByRole('region', { name: 'Needs attention' });
  await page.goto(`/${T}`);
  await expect(page.getByRole('region', { name: 'Repositories', exact: true })).toBeVisible();
  await expect(attention).toHaveCount(0);

  await g.mockApi(page, [
    [new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), (u: URL) => (u.searchParams.get('state') === 'open' ? g.pageOf([as(1, 'failed'), as(2, 'failed'), as(3, 'capped'), g.pull], 'next') : g.pageOf([g.pull]))],
    ...g.defaultApi(),
  ]);
  await page.reload();
  await expect(attention.getByRole('listitem')).toHaveText([/2\+ open pull requests whose last review failed/, /1\+ open pull requests whose last review hit a limit/]);
  await attention.getByRole('link', { name: /hit a limit/ }).click();
  await expect(page).toHaveURL(new RegExp(`${T}/pulls\\?outcome=capped$`));
  await expect(page.getByRole('combobox', { name: 'Last review outcome' })).toHaveValue('capped');
});

test('account overview says whether its connection receives webhooks', async ({ page }) => {
  const detail = g.golden<AccountDetail>('account_detail');
  const panel = page.getByRole('region', { name: 'Connection', exact: true });
  await page.goto(`/${T}`);
  await expect(panel).toContainText(detail.connection.name);
  await expect(panel).toContainText('receiving');
  await expect(panel.getByRole('note')).toHaveCount(0);

  await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}$`), { ...detail, connection: { ...detail.connection, lastWebhookAt: null } }], ...g.defaultApi()]);
  await page.reload();
  await expect(panel).toContainText('none yet');
  await expect(panel.getByRole('note')).toContainText(`GitHub App's webhook at ${g.meta.webUrl}${detail.connection.hookPath}`);
});

test('repositories filter and repository detail', async ({ page }) => {
  await page.goto(`/${T}/repos`);
  await expect(page.locator('tbody tr')).toHaveCount(1);
  await page.getByPlaceholder('Filter by name').fill('nomatch');
  await expect(page.locator('.state-msg')).toContainText('Nothing matches');
  await page.getByRole('button', { name: 'Clear filter' }).click();
  await expect(page.locator('tbody tr')).toHaveCount(1);
  await expect(page.getByPlaceholder('Filter by name')).toBeFocused();
  await page.getByPlaceholder('Filter by name').fill('alpha');
  await page.getByRole('link', { name: 'alpha/one' }).click();
  await expect(page).toHaveURL(new RegExp(`${T}/repos/alpha/one$`));
  await expect(page.locator('.deflist').first()).toContainText(g.repoDetail.settings.ignore[0]!);
  await expect(page.locator('.deflist').first()).toContainText(g.repoDetail.settings.mode);
  await expect(page.locator('#repo-index').locator('../..')).toContainText(String(g.repoDetail.indexRuns[0]!.chunkCount));
  await expect(page.locator('#repo-pulls').locator('../..')).toContainText(g.pull.title);
});

test('repository settings say where each comes from and what .kritik.yaml chose', async ({ page }) => {
  await page.goto(`/${T}/repos/alpha/one`);
  const settings = page.locator('#repo-settings').locator('../..');
  const rc = g.repoDetail.repoConfig!;
  // The golden file chose another review model; the admin's is shown beside it.
  await expect(settings.getByText(rc.settings.models.review, { exact: true })).toBeVisible();
  await expect(settings).toContainText(`(.kritik.yaml; the admin's is ${g.repoDetail.settings.models.review})`);
  await expect(settings).toContainText(`${g.repoDetail.settings.mode} (account)`);
  await expect(settings).toContainText('Settle 30s (default)');
  const file = page.locator('#repo-file').locator('../..');
  await expect(file).toContainText(rc.filter);
  await expect(file).toContainText(rc.dropped[0]!);
  await expect(file.getByRole('link', { name: 'the last review' })).toHaveAttribute('href', `#/a/${g.SLUG}/reviews/${rc.reviewId}`);
  await expect(page.locator('#repo-bounds').locator('../..')).toContainText(g.repoDetail.settings.allow.models!.join(', '));
});

test.describe('pulls list', () => {
  test('filters, load more and keyboard navigation', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    const rows = page.locator('.pull-rows .row');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText(`${g.pull.repository}#${g.pull.number}`);
    await expect(rows.first()).toContainText(`${g.pull.lastReview!.findings.blocking} blocking`);

    await page.getByRole('combobox', { name: 'State' }).selectOption('closed');
    await expect.poll(() => seen.some((u) => u.pathname.endsWith('/pulls') && u.searchParams.get('state') === 'closed')).toBe(true);
    await page.getByRole('combobox', { name: 'Last review outcome' }).selectOption('failed');
    await expect.poll(() => seen.some((u) => u.searchParams.get('outcome') === 'failed')).toBe(true);
    await page.getByPlaceholder('Search title').fill('widgets');
    await expect.poll(() => seen.some((u) => u.searchParams.get('q') === 'widgets')).toBe(true);

    await page.getByRole('button', { name: 'Load more' }).click();
    await expect.poll(() => seen.some((u) => u.searchParams.get('cursor') === g.repoPage.nextCursor)).toBe(true);
    await expect(rows).toHaveCount(2);

    // '?' in a focused select is the select's, not the help overlay's.
    await page.getByRole('combobox', { name: 'State' }).focus();
    await page.keyboard.press('?');
    await expect(page.locator('.help-overlay')).toHaveCount(0);

    await page.locator('h1').click();
    await page.keyboard.press('j');
    await expect(rows.first()).toHaveClass(/selected/);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(new RegExp(`${T}/pulls/alpha/one/7$`));
  });

  test('filters live in the URL: a reload keeps them, and Back from a pull returns to them', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    await page.getByRole('combobox', { name: 'State' }).selectOption('all');
    await page.getByRole('combobox', { name: 'Last review outcome' }).selectOption('failed');
    await page.getByPlaceholder('Search title').fill('wid gets');
    await expect(page).toHaveURL(new RegExp(`${T}/pulls\\?state=all&outcome=failed&q=wid\\+gets$`));

    await page.reload();
    await expect(page.getByRole('combobox', { name: 'State' })).toHaveValue('all');
    await expect(page.getByRole('combobox', { name: 'Last review outcome' })).toHaveValue('failed');
    await expect(page.getByPlaceholder('Search title')).toHaveValue('wid gets');
    const last = () => seen.filter((u) => u.pathname.endsWith('/pulls')).at(-1)?.searchParams;
    await expect.poll(() => last()?.get('q')).toBe('wid gets');
    expect(last()?.get('state')).toBe('all');
    expect(last()?.get('outcome')).toBe('failed');

    await page.locator('.pull-rows .row-link').first().click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls/alpha/one/7$`));
    await page.goBack();
    await expect(page).toHaveURL(/\?state=all&outcome=failed&q=wid\+gets$/);
    await expect(page.getByPlaceholder('Search title')).toHaveValue('wid gets');

    // The sidebar's link is the unfiltered list, search box included.
    await page.getByRole('navigation', { name: 'Account' }).getByRole('link', { name: 'Pulls' }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls$`));
    await expect(page.getByPlaceholder('Search title')).toHaveValue('');
    await expect(page.getByRole('combobox', { name: 'State' })).toHaveValue('open');
  });

  test('an empty list says whether its filters emptied it, and clears them', async ({ page }) => {
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), (u: URL) => g.pageOf(u.searchParams.has('outcome') ? [] : [g.pull])], ...g.defaultApi()]);
    await page.goto(`/${T}/pulls?outcome=failed`);
    await expect(page.locator('.state-msg')).toHaveText(/No pull requests match these filters\./);
    await page.getByRole('button', { name: 'Clear filters' }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls$`));
    await expect(page.locator('.pull-rows .row')).toHaveCount(1);
    await expect(page.getByPlaceholder('Search title')).toBeFocused();

    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), g.pageOf([])], ...g.defaultApi()]);
    await page.reload();
    await expect(page.locator('.state-msg')).toHaveText('No open pull requests.');
    await expect(page.getByRole('button', { name: 'Clear filters' })).toHaveCount(0);
  });

  test('the keyboard cursor stays on its pull when a live refetch adds one above it', async ({ page }) => {
    const newer = { ...g.pull, number: 9, title: 'Newer widgets', url: g.pull.url.replace(/\d+$/, '9') };
    let added = false;
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), () => g.pageOf(added ? [newer, g.pull] : [g.pull])], ...g.defaultApi()]);
    await page.goto(`/${T}/pulls`);
    const rows = page.locator('.pull-rows .row');
    await expect(rows).toHaveCount(1);
    await page.locator('h1').click();
    await page.keyboard.press('j');
    await expect(rows.first()).toHaveClass(/selected/);

    // The fixture's stream closes as it opens, and every reopen refetches.
    added = true;
    await expect(rows).toHaveCount(2);
    await expect(rows.first()).not.toHaveClass(/selected/);
    await expect(rows.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(new RegExp(`${T}/pulls/alpha/one/7$`));
  });
});

test('pull detail shows the review history and follow-ups with a transcript', async ({ page }) => {
  await page.goto(`/${T}/pulls/alpha/one/7`);
  await expect(page.locator('h1')).toContainText(g.pullDetail.pull.title);
  await expect(page.locator('.timeline-item')).toContainText('$0.42');
  await expect(page.locator('.timeline-item')).toContainText('excluded by filter');
  await expect(page.locator('.followup')).toContainText(g.followup.author);
  await page.getByRole('button', { name: 'Transcript' }).click();
  await expect(page.locator('.followup .turn')).toHaveCount(g.transcript.turns.length);
  await page.locator('.timeline-link').click();
  await expect(page).toHaveURL(new RegExp(`${T}/reviews/rev-1$`));
});

test.describe('review', () => {
  test('summary groups findings; tabs switch', async ({ page }) => {
    await page.goto(`/${T}/reviews/rev-1`);
    await expect(page.locator('#sum-take').locator('../..')).toContainText(g.reviewDetail.summary!.take);
    const f = g.reviewDetail.findings[0]!;
    await expect(page.locator(`#sev-${f.severity}`)).toBeVisible();
    await expect(page.locator('.finding')).toContainText(f.title);
    await expect(page.locator('.finding')).toContainText(`${f.path}:${f.line}-${f.endLine}`);
    await expect(page.locator('.finding .code-block')).toContainText(f.replacement);

    for (const [tab, text] of [
      ['Timeline', g.reviewDetail.runnerRun!.podName],
      ['Raw', '.kritik.yaml'],
      ['Usage', 'Total'],
    ] as const) {
      await page.locator('.tabs').getByRole('link', { name: tab, exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`/reviews/rev-1/${tab.toLowerCase()}$`));
      await expect(page.locator('.tab-panel')).toContainText(text);
    }
    await expect(page.locator('.tab.active')).toHaveText('Usage');
  });

  test('diff anchors a finding under its line', async ({ page }) => {
    await page.goto(`/${T}/reviews/rev-1/diff`);
    const anchored = page.locator('tr.dl-finding');
    await expect(anchored).toHaveCount(1);
    await expect(anchored).toContainText(g.reviewDetail.findings[0]!.title);
    // The row right above the finding is new-side line 3.
    await expect(anchored.locator('xpath=preceding-sibling::tr[1]')).toContainText('var x *int');
    await page.getByRole('button', { name: /a\.go/ }).click();
    await expect(page.locator('table.diff')).toHaveCount(0);
  });

  test('conversation shows tool calls as pretty JSON and truncated results', async ({ page }) => {
    await page.goto(`/${T}/reviews/rev-1/conversation`);
    const turn = page.locator('.turn').first();
    await expect(turn.locator('.tool-call pre')).toHaveText(JSON.stringify(g.transcript.turns[0]!.messages[0]!.toolCalls[0]!.input, null, 2));
    await expect(turn.locator('.badge-warn')).toContainText('truncated 10 B');
    await expect(turn).toContainText(g.transcript.turns[0]!.response.text);
    await page.getByRole('button', { name: /^System prompt/ }).click();
    await expect(page.locator('.conversation-top')).toContainText(g.transcript.system);
    await page.getByRole('button', { name: 'raw JSON' }).click();
    await expect(turn.locator('pre')).toContainText('"runnerRunId"');
    await page.getByPlaceholder('Filter turns').fill('no-such-text');
    await expect(page.locator('.turn')).toHaveCount(0);
  });
});

test('queue, usage, follow-ups and admin console pages render their fixtures', async ({ page }) => {
  const seen = await g.mockApi(page, g.defaultApi());
  await page.goto(`/${T}/queue`);
  await expect(page.locator('tbody tr')).toContainText(g.job.lastError);
  await expect(page.locator('tbody tr')).toContainText(`${g.job.attempt}/${g.job.maxAttempts}`);

  await page.goto(`/${T}/usage`);
  await expect(page.locator('tbody tr')).toContainText(g.usageSeries.rows[0]!.key);
  await expect(page.getByRole('img', { name: /Cost by day/ })).toBeVisible();
  await page.getByRole('button', { name: '7d' }).click();
  await page.getByRole('button', { name: 'model' }).click();
  await expect.poll(() => seen.some((u) => u.pathname.endsWith('/usage') && u.searchParams.get('group') === 'model')).toBe(true);

  await page.goto(`/${T}/followups`);
  await expect(page.locator('.followup')).toContainText(`${g.followup.repository}#${g.followup.number}`);

  await page.goto('/#/admin');
  await expect(page.getByRole('row').filter({ hasText: g.adminAccount.slug })).toContainText('not served');
});

test('a server-sent event for the account refetches the page', async ({ page }) => {
  const seen = await g.mockApi(page, g.defaultApi());
  await page.route('**/api/events', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      body: `event: ${g.liveEvent.kind}\ndata: ${JSON.stringify(g.liveEvent)}\n\n`,
    }),
  );
  await page.goto(`/${T}/queue`);
  await expect.poll(() => seen.filter((u) => u.pathname.endsWith('/queue')).length).toBeGreaterThan(1);
});

test('a stream that (re)opens refetches the page, event or not', async ({ page }) => {
  const seen = await g.mockApi(page, g.defaultApi());
  let opens = 0;
  await page.route('**/api/events', (route) => {
    opens++;
    return route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' });
  });
  await page.goto(`/${T}/queue`);
  const queues = () => seen.filter((u) => u.pathname.endsWith('/queue')).length;
  // The first fetch, then one refetch per open: the reconnect backoff
  // (at least 500ms) outlasts live()'s 300ms debounce, so none merge.
  await expect.poll(() => opens).toBeGreaterThan(1);
  await expect.poll(queues).toBeGreaterThanOrEqual(3);
});

test('the sidebar shows the version the server reports, below the admin console', async ({ page }) => {
  await page.goto(`/${T}`);
  const version = page.locator('aside.sidebar .sidebar-version');
  await expect(version).toHaveText(`kritik ${g.meta.version}`);
  const consoleLink = await page.getByRole('navigation', { name: 'Instance' }).boundingBox();
  const box = await version.boundingBox();
  expect(consoleLink && box && box.y >= consoleLink.y + consoleLink.height).toBe(true);
});

test('each page names itself in the browser tab', async ({ page }) => {
  const r = g.reviewDetail.review;
  for (const [h, title] of [
    ['#/', 'All accounts · kritik'],
    [T, `Overview · ${g.SLUG} · kritik`],
    [`${T}/repos/alpha/one`, 'alpha/one · kritik'],
    [`${T}/pulls?outcome=failed`, `Pull requests · ${g.SLUG} · kritik`],
    [`${T}/pulls/alpha/one/7`, `${g.pullDetail.pull.title} · alpha/one#7 · kritik`],
    [`${T}/reviews/rev-1/diff`, `${r.status} · ${r.pull.repository}#${r.pull.number} review · kritik`],
    [`${T}/queue`, `Queue · ${g.SLUG} · kritik`],
    ['#/admin', 'Admin console · kritik'],
  ]) {
    await page.goto(`/${h}`);
    await expect(page).toHaveTitle(title);
  }
});

test("a running review's tab title says when it is done", async ({ page }) => {
  let done = false;
  const review = (status: ReviewStatus) => ({ ...g.reviewDetail, review: { ...g.reviewDetail.review, status } });
  await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/reviews/rev-1$`), () => review(done ? 'completed' : 'running')], ...g.defaultApi()]);
  await page.goto(`/${T}/reviews/rev-1`);
  await expect(page).toHaveTitle(/^running · /);
  // The fixture's stream reopens, and every reopen refetches.
  done = true;
  await expect(page).toHaveTitle(/^completed · /);
});

test('a signed-in user navigating to sign-in is sent back', async ({ page, mockProviders }) => {
  await mockProviders();
  await page.goto(`/${T}/queue`);
  await expect(page.locator('tbody tr')).toHaveCount(1);
  await page.evaluate(() => (location.hash = '#/signin'));
  await expect(page).toHaveURL(/#\/$/);
  await expect(page.locator('.page-head h1')).toHaveText('All accounts');
  await expect(page.locator('.signin-card')).toHaveCount(0);
});

test('dark theme renders every page without console errors', async ({ page }) => {
  const errors: string[] = [];
  page.on('console', (m) => {
    if (m.type() === 'error') errors.push(m.text());
  });
  page.on('pageerror', (e) => errors.push(e.message));
  await page.addInitScript(() => localStorage.setItem('kritik-theme', 'dark'));
  for (const h of [T, `${T}/repos/alpha/one`, `${T}/pulls`, `${T}/reviews/rev-1/diff`, `${T}/reviews/rev-1/conversation`, `${T}/reviews/rev-1/timeline`, `${T}/usage`]) {
    await page.goto(`/${h}`);
    await expect(page.locator('.state-msg[aria-live]')).toHaveCount(0);
    await page.screenshot({ fullPage: true });
  }
  expect(await page.evaluate(() => document.documentElement.className)).toBe('dark');
  expect(errors).toEqual([]);
});

test.describe('pulls load more', () => {
  test('a Load more still in flight when the query changes is discarded', async ({ page }) => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    await g.mockApi(page, g.defaultApi());
    let held = false;
    await page.route((u) => u.pathname.endsWith('/pulls') && u.searchParams.has('cursor'), async (route) => {
      held = true;
      await gate;
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(g.pageOf([{ ...g.pull, number: 8, title: 'More widgets' }])) });
    });
    await page.goto(`/${T}/pulls`);
    await expect(page.locator('.pull-rows .row')).toHaveCount(1);
    await page.getByRole('button', { name: 'Load more' }).click();
    await expect.poll(() => held).toBe(true);
    await page.getByRole('combobox', { name: 'State' }).selectOption('all');
    await expect(page.locator('.pull-rows .row')).toHaveCount(1);
    release();
    await page.waitForTimeout(300);
    await expect(page.locator('.pull-rows')).not.toContainText('More widgets');
    await expect(page.locator('.pull-rows .row')).toHaveCount(1);
  });

  test('a live refetch keeps the pages already loaded', async ({ page }) => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const seen = await g.mockApi(page, g.defaultApi());
    await page.route('**/api/events', async (route) => {
      await gate;
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: `event: review\ndata: ${JSON.stringify(g.liveEvent)}\n\n`,
      });
    });
    await page.goto(`/${T}/pulls`);
    await page.getByRole('button', { name: 'Load more' }).click();
    await expect(page.locator('.pull-rows .row')).toHaveCount(2);
    const firstPages = () => seen.filter((u) => u.pathname.endsWith('/pulls') && !u.searchParams.has('cursor')).length;
    const before = firstPages();
    release();
    await expect.poll(firstPages).toBeGreaterThan(before);
    await expect(page.locator('.pull-rows .row')).toHaveCount(2);
    await expect(page.locator('.pull-rows')).toContainText('More widgets');
  });
});
