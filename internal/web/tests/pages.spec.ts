import { test, expect } from './fixtures';
import * as g from './golden';
import type { AccountDetail, Pull, ReviewStatus, Rule } from '../src/lib/types';

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

test('analytics shows the totals against the window before, the charts and the repositories', async ({ page }) => {
  const seen = await g.mockApi(page, g.defaultApi());
  await page.goto(`/${T}`);
  const stats = page.getByRole('region', { name: 'Totals' });
  const stat = (label: string) => stats.locator('.stat').filter({ has: page.getByText(label, { exact: true }) });
  const c = g.analytics.current;
  await expect(stat('Reviews').locator('.stat-value')).toHaveText(String(c.reviews));
  await expect(stat('Reviews').locator('.delta')).toHaveText('new');
  await expect(stat('Reviews').locator('.stat-sub')).toHaveText(`${c.failed} failed`);
  await expect(stat('Findings').locator('.stat-sub')).toHaveText(`${c.findings.blocking} blocking`);
  // 2 of 6 findings, where the window before had none to compare.
  await expect(stat('Addressed').locator('.stat-value')).toHaveText('33%');
  await expect(stat('Addressed').locator('.stat-sub')).toHaveText('2 of 6');
  await expect(stat('Addressed').locator('.delta')).toHaveCount(0);
  await expect(stat('Median review').locator('.stat-value')).toHaveText('1m 30s');
  await expect(stat('Time to merge').locator('.stat-value')).toHaveText('36h 0m');
  await expect(stat('Reactions').locator('.stat-value')).toHaveText(`${c.reactionsUp} up`);
  await expect(stat('Reactions').locator('.stat-sub')).toHaveText(`${c.reactionsDown} down`);
  await expect(stat('Spend').locator('.delta')).toHaveText('new');
  await expect(stat('Spend').locator('.delta')).toHaveClass(/tone-danger/);
  await expect(page.getByRole('img', { name: /^Completed reviews per day/ })).toBeVisible();
  const findings = page.getByRole('region', { name: 'Findings by severity' });
  await expect(findings.getByRole('list', { name: /legend/ }).getByRole('listitem')).toHaveText(['Blocking', 'Important', 'Nit']);
  await findings.getByRole('radio', { name: 'Table' }).click();
  await expect(findings.locator('tbody tr')).toHaveText([/Sep 1\s*1\s*2\s*3\s*6/]);
  await expect(page.getByRole('region', { name: 'Most reviewed repositories' }).locator('tbody tr')).toContainText(g.analytics.repositories[0]!.repository);

  await page.getByRole('radio', { name: '90 days' }).click();
  await expect.poll(() => seen.some((u) => u.pathname.endsWith('/analytics') && u.searchParams.get('group') === 'week')).toBe(true);
});

test('a chart reads one column at a time, by pointer or by keyboard', async ({ page }) => {
  await page.goto(`/${T}`);
  const chart = page.getByRole('img', { name: /^Completed reviews per day/ });
  await chart.focus();
  await page.keyboard.press('ArrowRight');
  const tip = page.locator('.chart-tip');
  await expect(tip).toContainText('Sep 1');
  await expect(tip).toContainText(new RegExp(`${g.analytics.series[0]!.reviews}\\s+Reviews`));
  await page.keyboard.press('Escape');
  await expect(tip).toHaveCount(0);
  await chart.hover();
  await expect(tip).toContainText('Sep 1');
});

test('account overview links the open pulls whose last review failed or was capped', async ({ page }) => {
  const as = (n: number, status: ReviewStatus): Pull => ({ ...g.pull, number: n, url: g.pull.url.replace(/\d+$/, String(n)), lastReview: { ...g.pull.lastReview!, status } });
  const attention = page.getByRole('region', { name: 'Needs attention' });
  await page.goto(`/${T}`);
  await expect(page.getByRole('region', { name: 'Most reviewed repositories' })).toBeVisible();
  await expect(attention).toHaveCount(0);

  await g.mockApi(page, [
    [new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), (u: URL) => (u.searchParams.get('state') === 'open' ? g.pageOf([as(1, 'failed'), as(2, 'failed'), as(3, 'capped'), g.pull], 'next') : g.pageOf([g.pull]))],
    ...g.defaultApi(),
  ]);
  await page.reload();
  await expect(attention.getByRole('listitem')).toHaveText([/2\+ open pull requests whose last review failed/, /1\+ open pull requests whose last review hit a limit/]);
  await attention.getByRole('link', { name: /hit a limit/ }).click();
  await expect(page).toHaveURL(new RegExp(`${T}/pulls\\?outcome=capped$`));
  await expect(page.getByRole('combobox', { name: 'Search pull requests' })).toHaveValue('status:capped');
});

test('analytics says when no webhook has reached the connection', async ({ page }) => {
  const detail = g.golden<AccountDetail>('account_detail');
  const panel = page.getByRole('region', { name: 'Connection', exact: true });
  await page.goto(`/${T}`);
  await expect(page.getByRole('region', { name: 'Totals' })).toBeVisible();
  await expect(panel).toHaveCount(0);

  await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}$`), { ...detail, connection: { ...detail.connection, lastWebhookAt: null } }], ...g.defaultApi()]);
  await page.reload();
  await expect(panel.getByRole('note')).toContainText(`GitHub App's webhook at ${g.meta.webUrl}${detail.connection.hookPath}`);
});

test("analytics says when the connection's webhooks arrive unsigned", async ({ page }) => {
  const detail = g.golden<AccountDetail>('account_detail');
  const later = new Date(Date.parse(detail.connection.lastWebhookAt!) + 60_000).toISOString();
  await g.mockApi(page, [
    [new RegExp(`/api/v1/accounts/${g.SLUG}$`), { ...detail, connection: { ...detail.connection, lastUnsignedWebhookAt: later } }],
    ...g.defaultApi(),
  ]);
  await page.goto(`/${T}`);
  const note = page.getByRole('region', { name: 'Connection', exact: true }).getByRole('note');
  await expect(note).toContainText('with no signature');
  await expect(note).toContainText("Set the GitHub App's webhook secret");
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
  await expect(page.locator('#repo-index').locator('../..')).toContainText(String(g.repoDetail.indexRuns[0]!.chunkCount));
  await expect(page.locator('#repo-pulls').locator('../..')).toContainText(g.pull.title);
});

test('repositories say which are forks or archived', async ({ page }) => {
  const copy = { ...g.repoPage.items[0]!, id: 'repo-2', fullName: 'alpha/copy', fork: true, archived: true };
  await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/repos$`), g.pageOf([g.repoPage.items[0]!, copy])], ...g.defaultApi()]);
  await page.goto(`/${T}/repos`);
  await expect(page.getByRole('row', { name: /alpha\/copy/ }).locator('.badge')).toHaveText(['fork', 'archived']);
  await expect(page.getByRole('row', { name: /alpha\/one/ }).locator('.badge')).toHaveCount(0);
});

test('repository settings say where each comes from and what .kritika.yaml chose', async ({ page }) => {
  await page.goto(`/${T}/repos/alpha/one`);
  const settings = page.locator('#repo-settings').locator('../..');
  const rc = g.repoDetail.repoConfig!;
  // The golden file chose another review model; the admin's is shown beside it.
  await expect(settings.getByText(rc.settings.models.review, { exact: true })).toBeVisible();
  await expect(settings).toContainText(`(.kritika.yaml; the admin's is ${g.repoDetail.settings.models.review})`);
  await expect(settings).toContainText('Forks skipped (account)');
  await expect(settings).toContainText('Settle 30s (default)');
  await expect(settings).toContainText(`Feedback ${g.repoDetail.settings.review.feedback} (default)`);
  await expect(settings).toContainText('AGENTS.md / CLAUDE.md read (default)');
  const file = page.locator('#repo-file').locator('../..');
  await expect(file).toContainText(rc.filter);
  await expect(file).toContainText(rc.dropped[0]!);
  await expect(file.getByRole('link', { name: 'the last review' })).toHaveAttribute('href', `#/a/${g.SLUG}/reviews/${rc.reviewId}`);
});

test.describe('pulls list', () => {
  test('filters, load more and keyboard navigation', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    const rows = page.locator('.pull-rows .pull-row');
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toContainText(`${g.pull.repository} · #${g.pull.number} · ${g.pull.author}`);
    await expect(rows.first()).toContainText(`${g.pull.lastReview!.findings.blocking} blocking`);

    await page.getByRole('radio', { name: 'Closed' }).click();
    await expect.poll(() => seen.some((u) => u.pathname.endsWith('/pulls') && u.searchParams.get('state') === 'closed')).toBe(true);
    await expect(page.getByRole('radio', { name: 'Closed' })).toHaveAttribute('aria-checked', 'true');
    await page.getByRole('combobox', { name: 'Search pull requests' }).fill('status:failed author:ada widgets');
    await expect.poll(() => seen.some((u) => u.searchParams.get('outcome') === 'failed' && u.searchParams.get('author') === 'ada' && u.searchParams.get('q') === 'widgets')).toBe(true);

    await page.getByRole('button', { name: 'Load more' }).click();
    await expect.poll(() => seen.some((u) => u.searchParams.get('cursor') === g.repoPage.nextCursor)).toBe(true);
    await expect(rows).toHaveCount(2);

    // '?' in the search box is the box's, not the help overlay's.
    await page.getByRole('combobox', { name: 'Search pull requests' }).focus();
    await page.keyboard.press('?');
    await expect(page.locator('.help-overlay')).toHaveCount(0);

    await page.locator('.key-hints').click();
    await page.keyboard.press('j');
    await expect(rows.first()).toHaveClass(/selected/);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(new RegExp(`${T}/pulls/alpha/one/7$`));
  });

  test('the search box suggests filter tokens and their values', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    const search = page.getByRole('combobox', { name: 'Search pull requests' });
    const options = page.getByRole('listbox', { name: 'Suggestions' }).getByRole('option');
    await search.focus();
    await expect(options).toHaveText([/^repo:/, /^author:/, /^status:/]);
    await expect(search).toHaveAttribute('aria-expanded', 'true');

    await search.pressSequentially('st');
    await expect(options).toHaveText([/^status:/]);
    await page.keyboard.press('Tab');
    await expect(search).toHaveValue('status:');
    await search.pressSequentially('fai');
    await expect(options).toHaveText(['status:failed']);
    await page.keyboard.press('ArrowDown');
    await expect(search).toHaveAttribute('aria-activedescendant', 'pull-search-suggest-0');
    await page.keyboard.press('Enter');
    await expect(search).toHaveValue('status:failed ');
    await expect.poll(() => seen.some((u) => u.pathname.endsWith('/pulls') && u.searchParams.get('outcome') === 'failed')).toBe(true);

    await search.pressSequentially('repo:');
    await options.filter({ hasText: g.repoPage.items[0]!.fullName }).click();
    await expect(search).toHaveValue(`status:failed repo:${g.repoPage.items[0]!.fullName} `);
    await expect.poll(() => seen.some((u) => u.searchParams.get('repo') === g.repoPage.items[0]!.fullName)).toBe(true);

    await page.keyboard.press('Escape');
    await expect(search).toHaveAttribute('aria-expanded', 'false');
  });

  test('a token that names no repository or status is not applied, and says so', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    await page.getByRole('combobox', { name: 'Search pull requests' }).fill('repo:nope/nope status:great');
    await expect(page.getByRole('note')).toHaveText('Not filtering by repo:nope/nope, status:great: nothing by that name.');
    await page.waitForTimeout(400);
    expect(seen.some((u) => u.searchParams.has('repo') || u.searchParams.has('outcome'))).toBe(false);
    await expect(page).toHaveURL(new RegExp(`${T}/pulls$`));
  });

  test('a click anywhere on a row opens its pull request', async ({ page }) => {
    await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    await page.locator('.pull-rows .pull-row').first().getByText('ada', { exact: false }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls/alpha/one/7$`));
  });

  test('filters live in the URL: a reload keeps them, and Back from a pull returns to them', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/pulls`);
    const search = page.getByRole('combobox', { name: 'Search pull requests' });
    await page.getByRole('radio', { name: 'All' }).click();
    await search.fill('wid gets status:failed');
    await expect(page).toHaveURL(new RegExp(`${T}/pulls\\?state=all&outcome=failed&q=wid\\+gets$`));

    // The box keeps what was typed rather than the URL's order.
    await expect(search).toHaveValue('wid gets status:failed');

    await page.reload();
    await expect(page.getByRole('radio', { name: 'All' })).toHaveAttribute('aria-checked', 'true');
    await expect(search).toHaveValue('status:failed wid gets');
    const last = () => seen.filter((u) => u.pathname.endsWith('/pulls')).at(-1)?.searchParams;
    await expect.poll(() => last()?.get('q')).toBe('wid gets');
    expect(last()?.get('state')).toBe('all');
    expect(last()?.get('outcome')).toBe('failed');

    await page.locator('.pull-rows .pull-title').first().click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls/alpha/one/7$`));
    await page.goBack();
    await expect(page).toHaveURL(/\?state=all&outcome=failed&q=wid\+gets$/);
    await expect(search).toHaveValue('status:failed wid gets');

    // The section's tab is the unfiltered list, search box included.
    await page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name: 'Pull requests' }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls$`));
    await expect(search).toHaveValue('');
    await expect(page.getByRole('radio', { name: 'Open' })).toHaveAttribute('aria-checked', 'true');
  });

  test('an empty list says whether its filters emptied it, and clears them', async ({ page }) => {
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), (u: URL) => g.pageOf(u.searchParams.has('outcome') ? [] : [g.pull])], ...g.defaultApi()]);
    await page.goto(`/${T}/pulls?outcome=failed`);
    await expect(page.locator('.state-msg')).toHaveText(/No pull requests match these filters\./);
    await page.getByRole('button', { name: 'Clear filters' }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/pulls$`));
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(1);
    await expect(page.getByRole('combobox', { name: 'Search pull requests' })).toBeFocused();

    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), g.pageOf([])], ...g.defaultApi()]);
    await page.reload();
    await expect(page.locator('.state-msg')).toHaveText('No open pull requests.');
    await expect(page.getByRole('button', { name: 'Clear filters' })).toHaveCount(0);
  });

  test("a fork's pull request not reviewed says it is reviewed on request", async ({ page }) => {
    const fork = { ...g.pull, number: 12, url: g.pull.url.replace(/\d+$/, '12'), fork: true, lastReview: null };
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), g.pageOf([fork, { ...g.pull, lastReview: null }])], ...g.defaultApi()]);
    await page.goto(`/${T}/pulls`);
    const rows = page.locator('.pull-rows .pull-row');
    await expect(rows.nth(0)).toContainText('fork, reviewed on request');
    await expect(rows.nth(0).getByText('fork, reviewed on request')).toHaveAttribute('title', 'A pull request from a fork is reviewed when a maintainer comments "@<bot> review" on it');
    await expect(rows.nth(1)).toContainText('not reviewed');
  });

  test('the keyboard cursor stays on its pull when a live refetch adds one above it', async ({ page }) => {
    const newer = { ...g.pull, number: 9, title: 'Newer widgets', url: g.pull.url.replace(/\d+$/, '9') };
    let added = false;
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/pulls$`), () => g.pageOf(added ? [newer, g.pull] : [g.pull])], ...g.defaultApi()]);
    await page.goto(`/${T}/pulls`);
    const rows = page.locator('.pull-rows .pull-row');
    await expect(rows).toHaveCount(1);
    await page.locator('.key-hints').click();
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

test.describe('findings', () => {
  test('lists each finding with its pull request and whether it was addressed', async ({ page }) => {
    await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/findings`);
    const f = g.accountFinding;
    const row = page.locator('.finding-row').first();
    await expect(row).toContainText(f.title);
    await expect(row).toContainText(f.explanation);
    await expect(row.locator('.sev')).toHaveText(f.severity);
    await expect(row.getByRole('link', { name: `${f.pull.repository} #${f.pull.number}` })).toHaveAttribute('href', `#/a/${g.SLUG}/pulls/alpha/one/7`);
    await expect(row.locator('.status-word')).toHaveText(f.status);
    await expect(row.getByTitle('Reactions to its comment on GitHub')).toHaveText(`${f.reactionsUp} ${f.reactionsDown}`);
    await expect(row.getByRole('link', { name: 'Thread on GitHub' })).toHaveAttribute('href', `${f.pull.url}#discussion_r${f.forgeCommentId}`);
    await expect(row.locator('.finding-rules').getByRole('link')).toHaveText(f.rules);
    await expect(page.locator('.sections .section-tab.active')).toHaveText('Analytics');
    await expect(page.getByRole('navigation', { name: 'Analytics' }).getByRole('link', { name: 'Findings' })).toHaveAttribute('aria-current', 'page');

    await row.locator('.finding-sub').first().click();
    await expect(page).toHaveURL(new RegExp(`${T}/reviews/${f.reviewId}$`));
  });

  test('filters by severity, status and repository tokens, in the URL', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/findings`);
    const search = page.getByRole('combobox', { name: 'Search findings' });
    await search.fill(`severity:blocking status:addressed repo:${g.repoPage.items[0]!.fullName} deref`);
    await expect(page).toHaveURL(new RegExp(`${T}/findings\\?severity=blocking&status=addressed&repo=alpha%2Fone&q=deref$`));
    const last = () => seen.filter((u) => u.pathname.endsWith('/findings')).at(-1)?.searchParams;
    await expect.poll(() => last()?.get('severity')).toBe('blocking');
    expect(last()?.get('status')).toBe('addressed');
    expect(last()?.get('repo')).toBe(g.repoPage.items[0]!.fullName);
    expect(last()?.get('q')).toBe('deref');

    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/findings$`), g.pageOf([])], ...g.defaultApi()]);
    await page.reload();
    await expect(search).toHaveValue(`repo:${g.repoPage.items[0]!.fullName} severity:blocking status:addressed deref`);
    await expect(page.locator('.state-msg')).toContainText('No findings match these filters.');
    await page.getByRole('button', { name: 'Clear filters' }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/findings$`));
    await expect(search).toHaveValue('');
    await expect(search).toBeFocused();
  });

  test('a cited rule narrows the list to the findings that cite it', async ({ page }) => {
    const seen = await g.mockApi(page, g.defaultApi());
    await page.goto(`/${T}/findings`);
    const id = g.accountFinding.rules[0]!;
    await page.locator('.finding-row').first().getByRole('link', { name: id }).click();
    await expect(page).toHaveURL(new RegExp(`${T}/findings\\?rule=${id}$`));
    await expect.poll(() => seen.filter((u) => u.pathname.endsWith('/findings')).at(-1)?.searchParams.get('rule')).toBe(id);
    await expect(page.getByRole('combobox', { name: 'Search findings' })).toHaveValue(`rule:${id}`);
  });
});

test.describe('rules', () => {
  test('lists each rule reviews check, where it is set and which repositories read it', async ({ page }) => {
    const file: Rule = { ...g.rule, kind: 'context', id: '', text: '', path: 'db/schema.sql', description: 'the schema', source: 'repository' };
    const fileRule: Rule = { ...g.rule, id: 'house-style', text: '', path: '.kritika/review.md', source: 'repository' };
    const renovate: Rule = { ...g.rule, id: 'renovate', text: 'Say what the update breaks.', paths: [], whenExpr: 'pr.headRef.startsWith("renovate/")' };
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/rules$`), [file, g.rule, fileRule, renovate]], ...g.defaultApi()]);
    await page.goto(`/${T}/rules`);
    const r = g.rule;
    const rows = page.locator('.rule-table tbody tr');
    await expect(rows).toHaveCount(4);
    // A rule with a whenExpr shows it in place of every change.
    await expect(rows.nth(2).locator('.rule-when code')).toHaveText(renovate.whenExpr);
    await expect(rows.nth(2)).not.toContainText('every change');
    // A file rule names its file over its id.
    await expect(rows.nth(1).locator('.rule-path')).toHaveText(fileRule.path);
    await expect(rows.nth(1).locator('.rule-sub')).toHaveText(fileRule.id);
    const row = rows.first();
    await expect(row.locator('.rule-body')).toHaveText(r.text);
    await expect(row.locator('.rule-sub')).toHaveText(r.id);
    await expect(row.locator('code')).toHaveText(r.paths);
    await expect(row).toContainText('Repository entry');
    await expect(row.getByRole('link', { name: r.repositories[0]! })).toHaveAttribute('href', `#/a/${g.SLUG}/repos/alpha/one`);
    // Its findings, narrowed to its one repository.
    await expect(row.locator('.rule-cited')).toContainText(`${r.addressed} addressed`);
    await expect(row.getByRole('link', { name: String(r.findings), exact: true })).toHaveAttribute(
      'href',
      `#/a/${g.SLUG}/findings?repo=alpha%2Fone&rule=${r.id}`,
    );
    await expect(rows.last().locator('.rule-path')).toHaveText(file.path);
    await expect(rows.last()).toContainText('.kritika.yaml');
    await expect(rows.last().locator('.rule-cited')).toHaveCount(0);
    await expect(page.locator('.sections .section-tab.active')).toHaveText('Rules');

    const search = page.getByRole('combobox', { name: 'Search rules' });
    await search.fill('kind:context package');
    await expect(page.locator('.state-msg')).toHaveText('No rule matches.');
    await search.fill('kind:rule package');
    await expect(rows).toHaveCount(1);
    await expect(rows.locator('.rule-sub')).toHaveText(r.id);
    await search.fill('renovate/');
    await expect(rows.locator('.rule-sub')).toHaveText(renovate.id);
  });

  test('says how to add a rule when there is none', async ({ page }) => {
    await g.mockApi(page, [[new RegExp(`/api/v1/accounts/${g.SLUG}/rules$`), []], ...g.defaultApi()]);
    await page.goto(`/${T}/rules`);
    await expect(page.locator('.state-msg')).toContainText('write them, or name files for them, under rules, and name context files under context');
  });
});

test('pull detail leads with its latest review, then the history and follow-ups with a transcript', async ({ page }) => {
  // The history also holds an earlier review that was skipped, which is the
  // only kind whose skip reason is shown.
  const first = g.pullDetail.reviews[0]!;
  await g.mockApi(page, [[/\/pulls\/alpha\/one\/7$/, { ...g.pullDetail, reviews: [first, { ...first, id: 'rev-0', status: 'skipped' }] }], ...g.defaultApi()]);
  await page.goto(`/${T}/pulls/alpha/one/7`);
  await expect(page.locator('h1')).toContainText(g.pullDetail.pull.title);
  const p = g.pullDetail.pull;
  await expect(page.locator('.page-head .meta-line')).toContainText(`${p.author} wants to merge ${p.headRef} into ${p.baseRef}`);
  const latest = page.getByRole('region', { name: 'Latest review' });
  await expect(latest).toContainText(g.reviewDetail.summary!.take);
  const f = g.reviewDetail.findings[0]!;
  await expect(latest.getByRole('list', { name: 'Findings' }).getByRole('listitem')).toHaveText([`${f.severity} ${f.title} ${f.path}:${f.line} Thread`]);
  await expect(latest.getByRole('link', { name: 'Thread' })).toHaveAttribute('href', `${p.url}#discussion_r${f.forgeCommentId}`);
  await expect(latest.getByRole('link', { name: 'Open the review' })).toHaveAttribute('href', `#/a/${g.SLUG}/reviews/rev-1`);
  await expect(page.locator('.timeline-item').first()).toContainText('$0.42');
  await expect(page.locator('.timeline-item').first()).not.toContainText('excluded by filter');
  await expect(page.locator('.timeline-item').nth(1)).toContainText('excluded by filter');
  await expect(page.locator('.followup')).toContainText(g.followup.author);
  await page.getByRole('button', { name: 'Transcript' }).click();
  await expect(page.locator('.followup .turn')).toHaveCount(g.transcript.turns.length);
  await page.locator('.timeline-link').first().click();
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
    await expect(page.locator('.finding-head .badge.mono')).toHaveText(f.rules);
    // The golden finding was posted inline as comment 55 on the pull request.
    await expect(page.locator('.finding').getByRole('link', { name: 'Thread on GitHub' })).toHaveAttribute(
      'href',
      `${g.reviewDetail.review.pull.url}#discussion_r${f.forgeCommentId}`,
    );
    const r = g.reviewDetail.review;
    const fact = (name: string) => page.locator('.page-head .facts > div').filter({ has: page.getByRole('term').getByText(name, { exact: true }) }).getByRole('definition');
    await expect(fact('Scope')).toHaveText(new RegExp(`^${r.scope}\\s\\(${r.scopeReason}\\)$`));
    await expect(fact('Model')).toHaveText(r.model);

    for (const [tab, text] of [
      ['Timeline', g.reviewDetail.runnerRun!.podName],
      ['Raw', '.kritika.yaml'],
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
    // The row right above the finding is new-side line 3, and the only one marked.
    await expect(anchored.locator('xpath=preceding-sibling::tr[1]')).toContainText('var x *int');
    await expect(page.locator('tr.dl-marked')).toHaveCount(1);
    await expect(page.locator('tr.dl-marked')).toContainText('var x *int');
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

test('pull detail says what its unfinished review job waits on, and nothing of one that simply runs', async ({ page }) => {
  await g.mockApi(page, g.defaultApi());
  await page.goto(`/${T}/pulls/alpha/one/7`);
  const j = g.job;
  const notice = page.locator('.job-notice');
  await expect(notice).toContainText(`A review is waiting to run again: attempt ${j.attempt} of ${j.maxAttempts} failed, the next is due at`);
  await expect(notice).toContainText(`GitHub did not answer. ${j.lastError}`);
  await expect(notice.getByRole('link', { name: 'Open the queue' })).toHaveAttribute('href', `#/a/${g.SLUG}/queue`);

  const pull = /\/pulls\/alpha\/one\/7$/;
  await g.mockApi(page, [[pull, { ...g.pullDetail, job: { ...j, state: 'running' } }], ...g.defaultApi()]);
  await page.reload();
  await expect(notice).toContainText(`A review is running, attempt ${j.attempt} of ${j.maxAttempts}.`);

  await g.mockApi(page, [[pull, { ...g.pullDetail, job: { ...j, state: 'available', attempt: 0, lastError: '', cause: '' } }], ...g.defaultApi()]);
  await page.reload();
  await expect(notice).toHaveText('A review is queued. Open the queue');

  await g.mockApi(page, [[pull, { ...g.pullDetail, job: { ...j, state: 'running', attempt: 1, lastError: '', cause: '' } }], ...g.defaultApi()]);
  await page.reload();
  await expect(page.locator('h1')).toContainText(g.pullDetail.pull.title);
  await expect(notice).toHaveCount(0);
});

test('queue, usage, follow-ups and admin console pages render their fixtures', async ({ page }) => {
  const seen = await g.mockApi(page, g.defaultApi());
  await page.goto(`/${T}/queue`);
  await expect(page.locator('tbody tr')).toContainText(`GitHub did not answer. ${g.job.lastError}`);
  await expect(page.locator('tbody tr')).toContainText(`${g.job.attempt}/${g.job.maxAttempts}`);

  await page.goto(`/${T}/usage`);
  await expect(page.locator('tbody tr')).toContainText(g.usageSeries.rows[0]!.key);
  await expect(page.getByRole('img', { name: /Cost by day/ })).toBeVisible();
  await expect(page.getByRole('meter', { name: 'Monthly tokens used' })).toHaveAttribute('aria-valuemax', String(g.accountSummary.usage.tokensPerMonth));
  await expect(page.getByRole('region', { name: 'This month' })).toContainText('$1.50');
  await page.getByRole('radio', { name: '7 days' }).click();
  await page.getByRole('radio', { name: 'Model' }).click();
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

test('the user menu shows the version the server reports', async ({ page }) => {
  await page.goto(`/${T}`);
  await page.locator('.user-menu summary').click();
  await expect(page.locator('.user-panel .user-version')).toHaveText(`kritika ${g.meta.version}`);
});

test('each page names itself in the browser tab', async ({ page }) => {
  const r = g.reviewDetail.review;
  for (const [h, title] of [
    ['#/', 'All accounts · kritika'],
    [T, `Analytics · ${g.SLUG} · kritika`],
    [`${T}/repos/alpha/one`, 'alpha/one · kritika'],
    [`${T}/pulls?outcome=failed`, `Pull requests · ${g.SLUG} · kritika`],
    [`${T}/pulls/alpha/one/7`, `${g.pullDetail.pull.title} · alpha/one#7 · kritika`],
    [`${T}/reviews/rev-1/diff`, `${r.status} · ${r.pull.repository}#${r.pull.number} review · kritika`],
    [`${T}/queue`, `Queue · ${g.SLUG} · kritika`],
    ['#/admin', 'Configuration · kritika'],
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
  await page.addInitScript(() => localStorage.setItem('kritika-theme', 'dark'));
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
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(1);
    await page.getByRole('button', { name: 'Load more' }).click();
    await expect.poll(() => held).toBe(true);
    await page.getByRole('radio', { name: 'All' }).click();
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(1);
    release();
    await page.waitForTimeout(300);
    await expect(page.locator('.pull-rows')).not.toContainText('More widgets');
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(1);
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
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(2);
    const firstPages = () => seen.filter((u) => u.pathname.endsWith('/pulls') && !u.searchParams.has('cursor')).length;
    const before = firstPages();
    release();
    await expect.poll(firstPages).toBeGreaterThan(before);
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(2);
    await expect(page.locator('.pull-rows')).toContainText('More widgets');
  });
});
