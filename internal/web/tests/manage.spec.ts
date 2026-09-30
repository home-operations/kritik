import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import * as g from './golden';
import type * as T from '../src/lib/types';

const S = g.SLUG;
const API = `/api/v1/accounts/${S}`;
const ADMIN = `#/a/${S}/admin`;

const adminMe: T.Me = { ...g.me, admin: true };
const memberMe: T.Me = { ...g.me, admin: false, accounts: [S] };

async function setup(page: Page, who: T.Me, rows: [RegExp, unknown][] = []): Promise<URL[]> {
  return g.mockApi(page, [[/\/api\/v1\/me$/, who], ...rows, ...g.defaultApi()]);
}

test('an account member cannot open the audit log', async ({ page }) => {
  await setup(page, memberMe);
  await page.goto(`/${ADMIN}/audit`);
  await expect(page.getByRole('alert')).toContainText('Only an admin');
  await expect(page.getByRole('navigation', { name: 'Settings' }).getByRole('link')).toHaveText(['Repositories']);
});

test('the audit log pages and expands detail', async ({ page }) => {
  const older: T.AuditEvent = { ...g.auditEvent, id: '6', action: 'review.rerun', target: 'rev-1', detail: {} };
  const seen = await setup(page, adminMe, [
    [new RegExp(`${API}/audit$`), (u: URL) => (u.searchParams.get('cursor') ? g.pageOf([older]) : g.pageOf([g.auditEvent], 'c1'))],
  ]);
  await page.goto(`/${ADMIN}/audit`);
  const rows = page.locator('table.audit tbody > tr');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText(g.auditEvent.action);
  await rows.first().getByRole('button', { name: 'detail' }).click();
  await expect(rows.first().locator('.detail-json')).toContainText('jobId');
  await page.getByRole('button', { name: 'Load more' }).click();
  await expect(rows).toHaveCount(2);
  expect(seen.some((u) => u.pathname.endsWith('/audit') && u.searchParams.get('cursor') === 'c1')).toBe(true);
  await expect(page.getByRole('button', { name: 'Load more' })).toHaveCount(0);
});

test.describe('actions', () => {
  test('an admin picks pull requests and re-runs them together', async ({ page }) => {
    const second = { ...g.pull, number: 9, title: 'Second', url: g.pull.url.replace(/\d+$/, '9') };
    await setup(page, adminMe, [[new RegExp(`${API}/pulls$`), g.pageOf([g.pull, second])]]);
    const sent = await g.mockWrites(page, [
      ['POST', new RegExp(`${API}/pulls/alpha/one/7/rerun$`), { status: 202, body: g.accepted }],
      ['POST', new RegExp(`${API}/pulls/alpha/one/9/rerun$`), g.apiError(409, 'already_queued', 'a review of this head is already queued or running')],
    ]);
    await page.goto(`/#/a/${S}/pulls`);
    await expect(page.getByRole('group', { name: 'Selected pull requests' })).toHaveCount(0);
    await page.getByRole('checkbox', { name: `Select ${g.pull.repository}#7` }).check();
    // Space picks the row the keyboard is on.
    await page.locator('.key-hints').click();
    await page.keyboard.press('j');
    await page.keyboard.press('j');
    await page.keyboard.press(' ');
    const bulk = page.getByRole('group', { name: 'Selected pull requests' });
    await expect(bulk).toContainText('2 selected');
    await expect(page.getByRole('checkbox', { name: 'Select every pull request shown' })).toBeChecked();

    await bulk.getByRole('button', { name: 'Re-run…' }).click();
    const dialog = page.getByRole('dialog', { name: 'Re-run 2 pull requests?' });
    await dialog.getByRole('button', { name: 'Re-run' }).click();
    await expect.poll(() => sent.length).toBe(2);
    await expect(page.getByRole('status')).toContainText('Re-run queued for 1 pull request, 1 already queued or running');
    await expect(bulk).toHaveCount(0);

    // A pick holds under its filter only.
    await page.getByRole('checkbox', { name: `Select ${g.pull.repository}#7` }).check();
    await expect(bulk).toContainText('1 selected');
    await page.getByRole('radio', { name: 'All' }).click();
    await expect(bulk).toHaveCount(0);
    await expect(page.getByRole('checkbox', { name: `Select ${g.pull.repository}#7` })).not.toBeChecked();
  });

  test('a member has no picking on the pull list', async ({ page }) => {
    await setup(page, memberMe);
    await page.goto(`/#/a/${S}/pulls`);
    await expect(page.locator('.pull-rows .pull-row')).toHaveCount(1);
    await expect(page.getByRole('checkbox')).toHaveCount(0);
    await expect(page.locator('.key-hints')).not.toContainText('select');
  });

  test('re-run and cancel on a review, re-run on a pull, reindex on a repository', async ({ page }) => {
    const running: T.ReviewDetail = { ...g.reviewDetail, review: { ...g.reviewDetail.review, status: 'running' } };
    await setup(page, adminMe, [[new RegExp(`${API}/reviews/rev-1$`), running]]);
    const sent = await g.mockWrites(page, [
      [
        'POST',
        new RegExp(`${API}/pulls/alpha/one/7/rerun$`),
        () =>
          sent.filter((s) => s.url.pathname.endsWith('/rerun')).length <= 1
            ? { status: 202, body: g.accepted }
            : g.apiError(409, 'already_queued', 'a review of this head is already queued or running'),
      ],
      ['POST', new RegExp(`${API}/reviews/rev-1/cancel$`), g.apiError(409, 'not_cancelable', 'the review is not running')],
      ['POST', new RegExp(`${API}/repos/alpha/one/reindex$`), g.apiError(503, 'actions_disabled', 'this process does not queue dashboard actions')],
    ]);

    await page.goto(`/#/a/${S}/reviews/rev-1`);
    await page.getByRole('button', { name: 'Re-run' }).click();
    const dialog = page.getByRole('dialog', { name: 'Re-run the review?' });
    await expect(dialog).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden();
    expect(sent).toHaveLength(0);
    await page.getByRole('button', { name: 'Re-run' }).click();
    await dialog.getByRole('button', { name: 'Re-run' }).click();
    await expect(page.getByRole('status')).toContainText(`Re-run queued, job #${g.accepted.jobId}`);

    await page.getByRole('button', { name: 'Cancel review' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Cancel review' }).click();
    await expect(page.getByRole('status')).toContainText('no longer running');

    await page.goto(`/#/a/${S}/pulls/alpha/one/7`);
    await page.getByRole('button', { name: 'Re-run' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Re-run' }).click();
    await expect.poll(() => sent.filter((s) => s.url.pathname.endsWith('/rerun')).length).toBe(2);
    await expect(page.getByRole('status')).toContainText('already queued or running');

    await page.goto(`/#/a/${S}/repos/alpha/one`);
    await page.getByRole('button', { name: 'Reindex' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Reindex' }).click();
    await expect(page.getByRole('status')).toContainText('cannot queue dashboard actions');
  });

  test('are hidden from an account member', async ({ page }) => {
    await setup(page, memberMe);
    await page.goto(`/#/a/${S}/reviews/rev-1`);
    await expect(page.locator('.page-head h1')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Re-run' })).toHaveCount(0);
    await page.goto(`/#/a/${S}/repos/alpha/one`);
    await expect(page.locator('#repo-settings')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Reindex' })).toHaveCount(0);
  });
});

test.describe('configuration page', () => {
  test('shows what setup still lacks, each with what to set in the configuration', async ({ page }) => {
    const fresh: T.SetupStatus = { ...g.setupStatus, connections: [], reviewModel: '', embedding: false };
    await setup(page, adminMe, [
      [/\/api\/v1\/admin\/setup$/, fresh],
      [/\/api\/v1\/admin\/accounts$/, []],
      [/\/api\/v1\/admin\/audit$/, g.pageOf([])],
    ]);
    await page.goto('/#/admin');
    const panel = page.locator('#op-setup').locator('../..');
    await expect(panel).toContainText('0 of 4 done');
    await expect(panel.getByRole('listitem')).toHaveText([
      /A GitHub App is connected Declare the App under apps/,
      /The App reaches a repository Install the App/,
      /A review model is set Set defaults.models.review/,
      /An embedder is set \(optional\) Set embedding/,
    ]);
    await expect(panel.locator('li.done')).toHaveCount(0);
    await expect(page.locator('input, select, textarea').and(page.locator('main *'))).toHaveCount(0);
  });

  test('marks each step done as the configuration covers it', async ({ page }) => {
    await setup(page, adminMe, [
      [/\/api\/v1\/admin\/accounts$/, [{ ...g.adminAccount, live: true, conflict: undefined }]],
      [/\/api\/v1\/admin\/audit$/, g.pageOf([])],
    ]);
    await page.goto('/#/admin');
    const panel = page.locator('#op-setup').locator('../..');
    await expect(panel).toContainText('3 of 4 done');
    await expect(panel.locator('li.done')).toHaveCount(3);
    await expect(panel.getByRole('listitem').last()).toContainText('Set embedding');
  });

  test("lists an App's installations and uninstalls it from an account nobody serves", async ({ page }) => {
    const conn = g.golden<T.AccountDetail>('account_detail').connection;
    const path = `/api/v1/admin/connections/${conn.name}/installations`;
    const served: T.AppInstallation = { ...g.appInstallation, id: 1, account: 'alpha', accountType: 'Organization', allRepositories: true, served: true };
    let reads = 0;
    await setup(page, adminMe, [
      [/\/api\/v1\/admin\/audit$/, g.pageOf([])],
      [new RegExp(`${path}$`), () => (reads++ === 0 ? [served, g.appInstallation] : [served])],
    ]);
    const sent = await g.mockWrites(page, [['DELETE', new RegExp(`${path}/${g.appInstallation.id}$`), { status: 204 }]]);
    await page.goto('/#/admin');
    const panel = page.locator('#op-connections').locator('../..');
    await expect(panel.getByRole('row').filter({ hasText: conn.name })).toContainText('config file');
    await panel.getByRole('button', { name: 'Installations' }).click();
    const table = panel.getByRole('table', { name: `Installations of ${conn.name}` });
    const stranger = table.getByRole('row').filter({ hasText: g.appInstallation.account });
    await expect(stranger).toContainText('not served');
    await expect(table.getByRole('button', { name: 'Uninstall from alpha' })).toHaveCount(0);
    await stranger.getByRole('button', { name: `Uninstall from ${g.appInstallation.account}` }).click();
    const dialog = page.getByRole('dialog', { name: 'Uninstall the App?' });
    await dialog.getByRole('button', { name: 'Uninstall' }).click();
    await expect.poll(() => sent.length).toBe(1);
    await expect(page.getByRole('status')).toContainText(`Uninstalled from ${g.appInstallation.account}`);
    await expect(stranger).toHaveCount(0);
  });

  test("flags an App whose webhooks arrive unsigned", async ({ page }) => {
    const conn = g.golden<T.AccountDetail>('account_detail').connection;
    const later = new Date(Date.parse(conn.lastWebhookAt!) + 60_000).toISOString();
    await setup(page, adminMe, [
      [/\/api\/v1\/admin\/audit$/, g.pageOf([])],
      [/\/api\/v1\/admin\/connections$/, [{ ...conn, lastUnsignedWebhookAt: later }]],
    ]);
    await page.goto('/#/admin');
    const row = page.locator('#op-connections').locator('../..').getByRole('row').filter({ hasText: conn.name });
    await expect(row).toContainText('unsigned');
    await expect(row).not.toContainText('receiving');
  });

  test('lists the instance settings read-only with their sources', async ({ page }) => {
    await setup(page, adminMe, [[/\/api\/v1\/admin\/audit$/, g.pageOf([])]]);
    await page.goto('/#/admin');
    const panel = page.locator('#op-instance').locator('../..');
    const row = panel.getByRole('row').filter({ hasText: g.instanceSetting.key });
    await expect(row).toContainText(g.instanceSetting.value);
    await expect(row).toContainText('config file');
    await expect(panel.locator('input, select, textarea')).toHaveCount(0);
  });

  test('lists an account no connection serves without a link, and why', async ({ page }) => {
    await setup(page, adminMe, [
      [/\/api\/v1\/admin\/audit$/, g.pageOf([])],
      [/\/api\/v1\/admin\/accounts$/, [{ ...g.adminAccount, live: true, conflict: undefined }, { ...g.adminAccount, slug: 'github/beta', connection: '' }]],
    ]);
    await page.goto('/#/admin');
    const live = page.getByRole('row').filter({ hasText: S });
    await expect(live).toHaveCount(1);
    await expect(live.getByRole('link', { name: S })).toBeVisible();
    await expect(live).toContainText(g.adminAccount.connection);
    const unserved = page.getByRole('row').filter({ hasText: 'github/beta' });
    await expect(unserved).toContainText(g.adminAccount.conflict!);
    await expect(unserved.getByRole('link')).toHaveCount(0);
  });
});

test.describe('settings search', () => {
  test('the palette finds a section of the Configuration page only when searching, and focuses it', async ({ page }) => {
    await setup(page, adminMe, [[/\/api\/v1\/admin\/audit$/, g.pageOf([])]]);
    await page.goto('/#/');
    await page.keyboard.press('ControlOrMeta+k');
    await expect(page.locator('.palette-row').filter({ hasText: 'Admin audit log' })).toHaveCount(0);
    await page.keyboard.type('installations');
    await expect(page.locator('.palette-row')).toHaveCount(1);
    await expect(page.locator('.palette-row')).toContainText('configuration');
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/#\/admin$/);
    await expect(page.locator('#op-connections')).toBeFocused();
  });

  test("a repository's settings filter to what matches", async ({ page }) => {
    await setup(page, adminMe);
    await page.goto(`/#/a/${S}/repos/alpha/one`);
    const settings = page.locator('#repo-settings').locator('../..');
    await expect(settings.getByText('Review model')).toBeVisible();
    await settings.getByLabel('Filter settings').fill('settle');
    await expect(settings.locator('dt')).toHaveText(['Settle']);
    await expect(page.locator('#repo-agent').locator('../..').locator('dt')).toHaveCount(0);
    await settings.getByLabel('Filter settings').fill('skipped');
    await expect(settings.locator('dt')).toHaveText(['Forks']);
    await settings.getByLabel('Filter settings').fill('nothing-like-it');
    await expect(settings).toContainText('No setting matches');
  });
});

test.describe('repository switches', () => {
  const REPOS = `#/a/${S}/repos`;
  const second: T.Repository = { ...g.repoPage.items[0]!, id: 'repo-2', fullName: 'alpha/two', enabled: false };
  const repos = [new RegExp(`${API}/repos$`), g.pageOf([g.repoPage.items[0]!, second])] as [RegExp, unknown];

  test('a member sees whether a repository is on, and nothing to change it', async ({ page }) => {
    await setup(page, memberMe, [repos]);
    await page.goto(`/${REPOS}`);
    await expect(page.locator('tbody tr')).toHaveCount(2);
    await expect(page.getByRole('switch')).toHaveCount(0);
    await expect(page.getByRole('checkbox')).toHaveCount(0);
  });

  // turnable serves the repository list with each switch's last PUT applied,
  // as the server's list reflects a choice at once.
  async function turnable(page: Page, list: T.Repository[]) {
    const rows = list.map((r) => ({ ...r }));
    await setup(page, adminMe, [[new RegExp(`${API}/repos$`), () => g.pageOf(rows)]]);
    return g.mockWrites(page, [
      [
        'PUT',
        /\/turned-on$/,
        (s) => {
          const name = decodeURIComponent(s.url.pathname.split('/repos/')[1]!.replace(/\/turned-on$/, ''));
          const r = rows.find((x) => x.fullName === name)!;
          const on = (s.body as T.TurnOnRequest).on;
          Object.assign(r, { turnedOn: on, enabled: on && !r.archived });
          return { status: 204 };
        },
      ],
      ['POST', /\/reindex$/, { status: 202, body: { jobId: 9 } }],
    ]);
  }

  test('a switch turns the repository on or off, one request each', async ({ page }) => {
    const sent = await turnable(page, [g.repoPage.items[0]!, second]);
    await page.goto(`/${REPOS}`);
    const one = page.getByRole('switch', { name: 'Review and index alpha/one' });
    await expect(one).toBeChecked();
    await one.uncheck();
    await expect(page.getByRole('status')).toContainText('1 repository turned off');
    await expect(one).not.toBeChecked();
    await page.getByRole('switch', { name: 'Review and index alpha/two' }).check();
    await expect.poll(() => sent.length).toBe(2);
    expect(sent.map((s) => `${s.method} ${s.url.pathname} ${JSON.stringify(s.body)}`)).toEqual([
      `PUT ${API}/repos/alpha/one/turned-on {"on":false}`,
      `PUT ${API}/repos/alpha/two/turned-on {"on":true}`,
    ]);
    await expect(page.getByRole('switch', { name: 'Review and index alpha/two' })).toBeChecked();
  });

  test('a fork is turned on like any repository, and an archived repository cannot be', async ({ page }) => {
    const copy: T.Repository = { ...g.repoPage.items[0]!, id: 'repo-3', fullName: 'alpha/copy', fork: true, enabled: false };
    const old: T.Repository = { ...g.repoPage.items[0]!, id: 'repo-4', fullName: 'alpha/old', archived: true, enabled: false };
    const sent = await turnable(page, [copy, old]);
    await page.goto(`/${REPOS}`);
    await expect(page.getByRole('switch', { name: 'Review and index alpha/old' })).toBeDisabled();
    await expect(page.getByRole('checkbox', { name: 'Select alpha/old' })).toHaveCount(0);
    await page.getByRole('switch', { name: 'Review and index alpha/copy' }).check();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]!.url.pathname).toBe(`${API}/repos/alpha/copy/turned-on`);
    expect(sent[0]!.body).toEqual({ on: true });
  });

  test('the list shows forks or archived repositories only when asked, and an admin can resync it', async ({ page }) => {
    const seen = await setup(page, adminMe, [repos]);
    const sent = await g.mockWrites(page, [['POST', /\/api\/v1\/admin\/connections\/[^/]+\/repositories$/, { status: 200, body: { added: 2 } }]]);
    await page.goto(`/${REPOS}`);
    const lists = () => seen.filter((u) => u.pathname.endsWith('/repos')).map((u) => u.searchParams.get('type'));
    await expect.poll(lists).toEqual([null]);
    await page.getByRole('combobox', { name: 'Type' }).selectOption('forks');
    await expect.poll(() => lists().at(-1)).toBe('forks');
    await expect(page.getByText('A fork is only reviewed and indexed once turned on here.')).toBeVisible();
    await page.getByRole('combobox', { name: 'Type' }).selectOption('archived');
    await expect.poll(() => lists().at(-1)).toBe('archived');

    await page.getByRole('button', { name: 'Resync from GitHub' }).click();
    await expect(page.getByRole('status')).toContainText('Resynced from GitHub: 2 repositories added');
    const detail = g.golden<T.AccountDetail>('account_detail');
    expect(sent.map((s) => s.url.pathname)).toEqual([`/api/v1/admin/connections/${detail.connection.name}/repositories`]);
  });

  test('selected repositories are turned off together, and the ones on reindexed', async ({ page }) => {
    const sent = await turnable(page, [g.repoPage.items[0]!, second]);
    await page.goto(`/${REPOS}`);
    await page.getByRole('checkbox', { name: 'Select every repository shown' }).check();
    const bulk = page.getByRole('group', { name: 'Selected repositories' });
    await expect(bulk).toContainText('2 selected');

    await bulk.getByRole('button', { name: 'Reindex…' }).click();
    const dialog = page.getByRole('dialog', { name: 'Reindex 1 repository?' });
    await expect(dialog).toContainText('1 repository that is off is skipped.');
    await dialog.getByRole('button', { name: 'Reindex' }).click();
    await expect(page.getByRole('status')).toContainText('Reindex queued for 1 repository');
    expect(sent.map((s) => `${s.method} ${s.url.pathname}`)).toEqual([`POST ${API}/repos/alpha/one/reindex`]);

    await page.getByRole('checkbox', { name: 'Select every repository shown' }).check();
    await bulk.getByRole('button', { name: 'Turn off' }).click();
    await expect(page.getByRole('status')).toContainText('2 repositories turned off');
    expect(sent.slice(1).map((s) => `${s.url.pathname} ${JSON.stringify(s.body)}`)).toEqual([
      `${API}/repos/alpha/one/turned-on {"on":false}`,
      `${API}/repos/alpha/two/turned-on {"on":false}`,
    ]);
    await expect(page.getByRole('switch', { name: 'Review and index alpha/one' })).not.toBeChecked();
  });
});
