import { test, expect } from './fixtures';
import * as g from './golden';

const T = `#/t/${g.SLUG}`;

test.beforeEach(async ({ page }) => {
  await g.mockApi(page, g.defaultApi());
});

test.describe('overview', () => {
  test('a single-tenant member stays on the breakdown instead of leaving for the tenant', async ({ page }) => {
    await page.goto('/#/');
    await expect(page.locator('.page-head h1')).toHaveText('All tenants');
    await expect(page).toHaveURL(/#\/$/);
    const rows = page.locator('table.tenant-breakdown tbody tr');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText(g.tenantSummary.slug);
    await rows.first().getByRole('link', { name: g.tenantSummary.slug }).click();
    await expect(page).toHaveURL(new RegExp(`#/t/${g.tenantSummary.slug}$`));
  });

  test('several tenants each get a row, and the tiles add them up', async ({ page }) => {
    await g.mockApi(page, [[/\/api\/v1\/tenants$/, [g.tenantSummary, { ...g.tenantSummary, slug: 'beta' }]], ...g.defaultApi()]);
    await page.goto('/#/');
    await expect(page.locator('table.tenant-breakdown tbody tr')).toHaveCount(2);
    const tiles = page.getByRole('region', { name: 'Across all tenants' });
    await expect(tiles.locator('.tile').filter({ hasText: 'Reviews, last 7 days' })).toContainText(String(2 * g.tenantSummary.reviews7d));
    await expect(tiles.locator('.tile').filter({ hasText: 'Repositories' })).toContainText(String(2 * g.tenantSummary.repositories));
    await expect(tiles.locator('.tile').filter({ hasText: 'Spend this month' })).toContainText('$3.00');
  });
});

test('tenant overview shows tiles, recent reviews, queue and repositories', async ({ page }) => {
  await page.goto(`/${T}`);
  await expect(page.locator('.tile').first()).toContainText(String(g.tenantSummary.reviews7d));
  await expect(page.locator('.tiles')).toContainText('$1.50');
  await expect(page.getByRole('meter', { name: 'Monthly tokens used' })).toHaveAttribute('aria-valuemax', String(g.tenantSummary.usage.tokensPerMonth));
  await expect(page.locator('#ov-recent').locator('..').locator('..')).toContainText(g.pull.title);
  await expect(page.locator('.chips')).toContainText(`1 ${g.job.state}`);
  await expect(page.locator('table.data')).toContainText(g.repoPage.items[0]!.fullName);
});

test('repositories filter and repository detail', async ({ page }) => {
  await page.goto(`/${T}/repos`);
  await expect(page.locator('tbody tr')).toHaveCount(1);
  await page.getByPlaceholder('Filter by name').fill('nomatch');
  await expect(page.locator('.state-msg')).toContainText('Nothing matches');
  await page.getByPlaceholder('Filter by name').fill('alpha');
  await page.getByRole('link', { name: 'alpha/one' }).click();
  await expect(page).toHaveURL(new RegExp(`${T}/repos/alpha/one$`));
  await expect(page.locator('.deflist').first()).toContainText(g.repoDetail.settings.ignore[0]!);
  await expect(page.locator('.deflist').first()).toContainText(g.repoDetail.settings.mode);
  await expect(page.locator('#repo-index').locator('../..')).toContainText(String(g.repoDetail.indexRuns[0]!.chunkCount));
  await expect(page.locator('#repo-pulls').locator('../..')).toContainText(g.pull.title);
});

test('a repository several installations hold asks which one, then loads it', async ({ page }) => {
  const detail = new RegExp(`/api/v1/tenants/${g.SLUG}/repos/alpha/one$`);
  await page.route(
    (u) => detail.test(u.pathname),
    (route) => {
      const installation = new URL(route.request().url()).searchParams.get('installation');
      if (!installation) {
        const body = { code: 'ambiguous', message: 'several installations hold this repository', details: { installations: ['alpha-forgejo', 'alpha-github'] } };
        return route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify(body) });
      }
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ...g.repoDetail, installation }) });
    },
  );
  await page.goto(`/${T}/repos/alpha/one`);
  await expect(page.getByRole('heading', { name: 'Which installation?' })).toBeVisible();
  await page.getByRole('link', { name: 'alpha-github' }).click();
  await expect(page).toHaveURL(new RegExp(`${T}/repos/alpha/one\\?installation=alpha-github$`));
  await expect(page.locator('.deflist').first()).toContainText('alpha-github');
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

    await page.locator('h1').click();
    await page.keyboard.press('j');
    await expect(rows.first()).toHaveClass(/selected/);
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

test('queue, usage, follow-ups and operator pages render their fixtures', async ({ page }) => {
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

  await page.goto('/#/operator');
  await expect(page.locator('tbody tr')).toContainText('not live');
});

test('a server-sent event for the tenant refetches the page', async ({ page }) => {
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

test('a signed-in user navigating to sign-in is sent back', async ({ page, mockProviders }) => {
  await mockProviders();
  await page.goto(`/${T}/queue`);
  await expect(page.locator('tbody tr')).toHaveCount(1);
  await page.evaluate(() => (location.hash = '#/signin'));
  await expect(page).toHaveURL(/#\/$/);
  await expect(page.locator('.page-head h1')).toHaveText('All tenants');
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
