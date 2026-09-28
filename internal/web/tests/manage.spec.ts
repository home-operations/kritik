import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import * as g from './golden';
import type * as T from '../src/lib/types';

const S = g.SLUG;
const API = `/api/v1/accounts/${S}`;
const ADMIN = `#/a/${S}/admin`;

const adminMe: T.Me = { ...g.me, operator: false, accounts: [{ slug: S, role: 'admin', managedBy: 'dashboard' }] };
const memberMe: T.Me = { ...g.me, operator: false, accounts: [{ slug: S, role: 'member', managedBy: 'dashboard' }] };
const operatorMe: T.Me = { ...g.me, operator: true };

// The golden config, with enough spec to exercise every part of the form.
const inst0 = g.accountConfig.spec.connections as Record<string, unknown>[];
const dashboardConfig: T.AccountConfig = {
  ...g.accountConfig,
  spec: {
    ...g.accountConfig.spec,
    limits: { concurrency: 2 },
    connections: [
      { ...inst0[0], forge: 'github', accounts: ['bot'], app: { clientId: 'Iv1.bot', privateKey: { set: true }, webhookSecret: { set: true } } },
    ],
    repositories: [{ name: 'alpha/one', mode: 'agentic', agent: { maxSteps: 10 }, konflate: 'keep-me' }],
  },
};

async function setup(page: Page, who: T.Me, rows: [RegExp, unknown][] = []): Promise<URL[]> {
  return g.mockApi(page, [[/\/api\/v1\/me$/, who], ...rows, ...g.defaultApi()]);
}

function configRow(cfg: T.AccountConfig | ((u: URL) => T.AccountConfig)): [RegExp, unknown] {
  return [new RegExp(`${API}/config$`), cfg];
}

test.describe('account configuration', () => {
  test('a file account renders read-only with secrets as set/not set', async ({ page }) => {
    const file: T.AccountConfig = { ...dashboardConfig, managedBy: 'file', revision: null, editable: false };
    await setup(page, adminMe, [configRow(file)]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('note')).toContainText('declared in the configuration file');
    await expect(page.locator('.spec-view')).toContainText('alpha-bot');
    await expect(page.locator('.spec-view .pill').first()).toHaveText('set');
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
  });

  test('keep, replace and generate secret controls shape the PUT, and the generated secret shows once', async ({ page }) => {
    let reads = 0;
    const seen = await setup(page, adminMe, [
      configRow(() => (reads++ === 0 ? dashboardConfig : { ...dashboardConfig, revision: 4 })),
    ]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: g.accountWriteResult }]]);
    await page.goto(`/${ADMIN}/config`);

    const key = page.locator('[data-path="connections[0].app.privateKey"]');
    await expect(key.getByLabel('Keep current')).toBeChecked();
    // A password field is never pre-filled, including after switching away and back.
    await key.getByLabel('Replace with a new value').check();
    await key.getByLabel('App private key: new value').fill('typed-then-dropped');
    await key.getByLabel('Keep current').check();
    await key.getByLabel('Replace with a new value').check();
    await expect(key.getByLabel('App private key: new value')).toHaveValue('');
    await key.getByLabel('App private key: new value').fill('key-new');
    await page.locator('[data-path="connections[0].app.webhookSecret"]').getByLabel('Generate').check();

    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const body = sent[0]!.body as T.UpdateAccountRequest;
    expect(body.revision).toBe(3);
    const app = (body.spec.connections as Record<string, unknown>[])[0]!.app as Record<string, unknown>;
    expect(app.privateKey).toEqual({ value: 'key-new' });
    expect(app.webhookSecret).toEqual({ generate: true });
    expect(app).not.toHaveProperty('clientIdFrom');
    expect(body.spec.limits).toEqual({ concurrency: 2 });
    expect(body.spec.repositories).toEqual([{ name: 'alpha/one', mode: 'agentic', agent: { maxSteps: 10 }, konflate: 'keep-me' }]);

    const dialog = page.getByRole('dialog', { name: 'Generated webhook secrets' });
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText('only time');
    await expect(dialog.getByTestId('generated-secret')).toHaveText('00ff');
    await expect(dialog).toContainText('/hooks/alpha-bot');
    await dialog.getByRole('button', { name: 'I have copied them' }).click();
    await expect(dialog).toBeHidden();
    await expect(page.getByTestId('generated-secret')).toHaveCount(0);
    // The Save button that opened it was remounted away; focus lands on the panel heading.
    await expect(page.locator('#admin-config')).toBeFocused();
    // Saved: the config reloads and the typed secret is gone with the old draft.
    await expect(page.locator('#admin-config').locator('..')).toContainText('revision 4');
    expect(seen.filter((u) => u.pathname.endsWith('/config')).length).toBeGreaterThanOrEqual(2);
    await expect(page.locator('[data-path="connections[0].app.privateKey"]').getByLabel('Keep current')).toBeChecked();
    await expect(page.locator('input[type=password]')).toHaveCount(0);
  });

  test('fields left empty show what they inherit, and from where', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    await page.goto(`/${ADMIN}/config`);
    const inh = g.accountConfig.inherited;
    await expect(page.locator('[data-path="models.review"]')).toHaveAttribute('placeholder', `inherits ${inh.account.models.review} from the config file`);
    await expect(page.locator('[data-path="settle"]')).toHaveAttribute('placeholder', "inherits 30s from kritik's default");
    await expect(page.locator('[data-path="repositories[0].mode"] option[value=""]')).toHaveText(`default: ${inh.repository.mode}`);
    await expect(page.locator('[data-path="repositories[0].filter"]')).toHaveAttribute('placeholder', `inherits ${inh.repository.filter} from kritik's default`);
  });

  test('a 422 highlights and focuses the field its path names', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const accounts = 'connections[0].accounts';
    const sent = await g.mockWrites(page, [
      [
        'PUT',
        new RegExp(`${API}/config$`),
        () =>
          sent.length <= 2
            ? g.apiError(422, 'invalid_spec', `${accounts}: the account is not allowed`, { path: accounts })
            : g.apiError(422, 'reenter_secret', 'enter this secret again', { path: 'connections[0].app.privateKey' }),
      ],
    ]);
    await page.goto(`/${ADMIN}/config`);
    await page.locator(`[data-path="${accounts}"]`).fill('bot\nelsewhere');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('the account is not allowed');
    await expect(page.locator(`[data-path="${accounts}"]`)).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator(`[data-path="${accounts}"]`)).toBeFocused();
    // The same error again still moves focus back to the field.
    await page.getByLabel('Filter').first().focus();
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(2);
    await expect(page.locator(`[data-path="${accounts}"]`)).toBeFocused();

    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('must be entered again');
    await expect(page.locator('[data-path="connections[0].app.privateKey"]')).toHaveClass(/invalid/);
    await expect(page.locator(`[data-path="${accounts}"]`)).not.toHaveAttribute('aria-invalid', 'true');
    // Adding or removing an item shifts indexes, so it dismisses a path error.
    await page.getByRole('button', { name: 'Add repository' }).click();
    await expect(page.locator('[data-path="connections[0].app.privateKey"]')).not.toHaveClass(/invalid/);
    await expect(page.locator('.form-alert')).toHaveCount(0);
  });

  test('an admin adds a provider key and sets the review model on it', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByRole('button', { name: 'Add provider key' }).click();
    await page.locator('[data-path="providers..name"]').fill('mine');
    await page.locator('[data-path="providers.mine.type"]').selectOption('anthropic');
    await page.locator('[data-path="providers.mine.apiKey"]').getByLabel('API key: new value').fill('sk-test');
    await page.locator('[data-path="models.review"]').fill('mine/claude');
    await page.getByRole('button', { name: 'Save' }).click();

    await expect.poll(() => sent.length).toBe(1);
    const spec = (sent[0]!.body as T.UpdateAccountRequest).spec;
    expect(spec.providers).toEqual({ mine: { type: 'anthropic', apiKey: { value: 'sk-test' } } });
    expect((spec.models as Record<string, unknown>).review).toBe('mine/claude');
  });

  test('a provider key is not kept once its endpoint changes', async ({ page }) => {
    const withKey: T.AccountConfig = {
      ...dashboardConfig,
      spec: { ...dashboardConfig.spec, providers: { mine: { type: 'openai', apiKey: { set: true } } } },
    };
    await setup(page, adminMe, [configRow(withKey)]);
    await page.goto(`/${ADMIN}/config`);
    const key = page.locator('[data-path="providers.mine.apiKey"]');
    await expect(key.getByLabel('Keep current')).toBeChecked();
    await page.locator('[data-path="providers.mine.baseUrl"]').fill('https://llm.example/v1');
    await expect(key.getByLabel('Keep current')).toHaveCount(0);
    await expect(page.getByRole('note').filter({ hasText: 'cannot be kept' })).toBeVisible();
  });

  test('a renamed connection cannot keep the secrets stored under its new name', async ({ page }) => {
    const b = (dashboardConfig.spec.connections as Record<string, unknown>[])[0]!;
    const two: T.AccountConfig = {
      ...dashboardConfig,
      spec: { ...dashboardConfig.spec, connections: [b, { ...b, name: 'beta-bot' }] },
    };
    await setup(page, adminMe, [configRow(two)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByRole('button', { name: 'Remove connection' }).first().click();
    await page.locator('[data-path="connections[0].name"]').fill('alpha-bot');
    await expect(page.getByRole('note').filter({ hasText: 'Renamed from' })).toContainText('beta-bot');
    const key = page.locator('[data-path="connections[0].app.privateKey"]');
    await expect(key.getByLabel('Keep current')).toHaveCount(0);
    await expect(page.locator('[data-path="connections[0].app.webhookSecret"]').getByLabel('Generate')).toBeChecked();
    await key.getByLabel('App private key: new value').fill('fresh');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const insts = (sent[0]!.body as T.UpdateAccountRequest).spec.connections as Record<string, unknown>[];
    expect(insts).toHaveLength(1);
    expect(insts[0]!.name).toBe('alpha-bot');
    const app = insts[0]!.app as Record<string, unknown>;
    expect(app.privateKey).toEqual({ value: 'fresh' });
    expect(app.webhookSecret).toEqual({ generate: true });
    expect(JSON.stringify(insts[0])).not.toContain('keep');
  });

  test('switching to JSON with a typed secret is refused, so the typed value is still what saves', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    const key = page.locator('[data-path="connections[0].app.privateKey"]');
    await key.getByLabel('Replace with a new value').check();
    await key.getByLabel('App private key: new value').fill('typed');
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    await expect(page.locator('.form-alert')).toContainText('JSON view never shows them');
    await expect(page.getByLabel('Spec JSON')).toHaveCount(0);
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const inst = ((sent[0]!.body as T.UpdateAccountRequest).spec.connections as Record<string, unknown>[])[0]!;
    expect((inst.app as Record<string, unknown>).privateKey).toEqual({ value: 'typed' });
  });

  test('a revision conflict offers to reload the latest', async ({ page }) => {
    let reads = 0;
    await setup(page, adminMe, [configRow(() => (reads++ === 0 ? dashboardConfig : { ...dashboardConfig, revision: 9 }))]);
    await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), g.apiError(409, 'revision_conflict', 'the account was changed')]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByLabel('Filter').first().fill('changed');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('Someone else saved this account');
    await page.getByRole('button', { name: /Reload the latest/ }).click();
    await expect(page.locator('#admin-config').locator('..')).toContainText('revision 9');
    await expect(page.getByLabel('Filter').first()).toHaveValue('');
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('the JSON editor shows secrets as keep and saves the JSON as written', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    const key = page.locator('[data-path="connections[0].app.privateKey"]');
    await key.getByLabel('Replace with a new value').check();
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    const box = page.getByLabel('Spec JSON');
    const text = await box.inputValue();
    const spec = JSON.parse(text) as Record<string, unknown>;
    expect(((spec.connections as Record<string, unknown>[])[0]!.app as Record<string, unknown>).privateKey).toEqual({ keep: true });

    await box.fill('{ not json');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.locator('.form-alert')).toContainText('does not parse');
    expect(sent).toHaveLength(0);

    await box.fill(JSON.stringify({ ...spec, filter: 'from-json' }));
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect((sent[0]!.body as T.UpdateAccountRequest).spec.filter).toBe('from-json');
  });

  test('unsaved edits ask before leaving', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig), [new RegExp(`${API}/audit$`), g.pageOf([])]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByLabel('Filter').first().fill('draft');
    page.once('dialog', (d) => void d.dismiss());
    await page.getByRole('link', { name: 'Audit log' }).click();
    await expect(page).toHaveURL(new RegExp(`${ADMIN}/config$`));
    await expect(page.getByLabel('Filter').first()).toHaveValue('draft');
    page.once('dialog', (d) => void d.accept());
    await page.getByRole('link', { name: 'Audit log' }).click();
    await expect(page).toHaveURL(new RegExp(`${ADMIN}/audit$`));
  });

  test('management disabled makes the config read-only and hides account creation', async ({ page }) => {
    await setup(page, operatorMe, [[/\/api\/v1\/meta$/, { ...g.meta, management: false }], configRow(dashboardConfig)]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('note')).toContainText('no sealing key configured');
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
    await page.goto('/#/operator');
    await expect(page.getByRole('note')).toContainText('no sealing key configured');
    await expect(page.getByRole('button', { name: 'New account' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: /Delete account/ })).toHaveCount(0);
  });

  test('an account member cannot open the admin page', async ({ page }) => {
    await setup(page, memberMe);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('alert')).toContainText('Only an admin');
    await expect(page.getByRole('link', { name: 'Admin' })).toHaveCount(0);
  });
});

test('the audit log pages and expands detail', async ({ page }) => {
  const older: T.AuditEvent = { ...g.auditEvent, id: '6', action: 'account.create', target: g.SLUG, detail: {} };
  const seen = await setup(page, adminMe, [
    [new RegExp(`${API}/audit$`), (u: URL) => (u.searchParams.get('cursor') ? g.pageOf([older]) : g.pageOf([g.auditEvent], 'c1'))],
  ]);
  await page.goto(`/${ADMIN}/audit`);
  const rows = page.locator('table.audit tbody > tr');
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText(g.auditEvent.action);
  await rows.first().getByRole('button', { name: 'detail' }).click();
  await expect(rows.first().locator('.detail-json')).toContainText('secretsChanged');
  await page.getByRole('button', { name: 'Load more' }).click();
  await expect(rows).toHaveCount(2);
  expect(seen.some((u) => u.pathname.endsWith('/audit') && u.searchParams.get('cursor') === 'c1')).toBe(true);
  await expect(page.getByRole('button', { name: 'Load more' })).toHaveCount(0);
});

test.describe('actions', () => {
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

test.describe('admin console', () => {
  test('creates an account', async ({ page }) => {
    await setup(page, operatorMe);
    const sent = await g.mockWrites(page, [
      ['POST', /\/api\/v1\/accounts$/, { status: 201, body: { ...g.accountWriteResult, slug: 'beta', generated: { 'connections[beta-bot].app.webhookSecret': 'abcd' } } }],
    ]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'New account' }).click();
    await page.getByLabel('Slug').fill('beta');
    await page.getByRole('button', { name: 'Add connection' }).click();
    await page.getByLabel('Name', { exact: true }).fill('beta-bot');
    await expect(page.getByLabel('Forge')).toHaveValue('github');
    for (const other of ['github-enterprise', 'gitlab', 'forgejo', 'gitea']) {
      await expect(page.getByLabel('Forge').locator(`option[value="${other}"]`)).toHaveJSProperty('disabled', true);
    }
    await page.locator('[data-path="connections[0].accounts"]').fill('bot\n  other-org \n\n');
    await page.getByLabel('App client ID', { exact: true }).fill('Iv1.beta');
    await page.getByLabel('App private key: new value').fill('key');
    await page.getByLabel('Concurrency').fill('3');
    await page.getByRole('button', { name: 'Create account' }).click();

    await expect.poll(() => sent.length).toBe(1);
    const body = sent[0]!.body as T.CreateAccountRequest;
    expect(body.slug).toBe('beta');
    expect(body.spec).toEqual({
      slug: 'beta',
      connections: [
        {
          name: 'beta-bot',
          forge: 'github',
          accounts: ['bot', 'other-org'],
          app: { clientId: 'Iv1.beta', privateKey: { value: 'key' }, webhookSecret: { generate: true } },
        },
      ],
      limits: { concurrency: 3 },
    });
    await expect(page.getByRole('dialog', { name: 'Generated webhook secrets' })).toContainText('/hooks/beta-bot');
  });

  test('offers adopt only for a slug a gone account used, and sends it', async ({ page }) => {
    await setup(page, operatorMe);
    const sent = await g.mockWrites(page, [
      [
        'POST',
        /\/api\/v1\/accounts$/,
        (s) => {
          const n = sent.length;
          if (n === 1) return g.apiError(409, 'slug_taken', 'a dashboard account with this slug already exists', { path: 'slug' });
          if (n === 2) return g.apiError(409, 'slug_taken', 'an account used this slug before', { path: 'slug', adoptable: true });
          return (s.body as T.CreateAccountRequest).adopt ? { status: 201, body: g.accountWriteResult } : g.apiError(409, 'slug_taken', 'again', { path: 'slug', adoptable: true });
        },
      ],
    ]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'New account' }).click();
    await page.getByLabel('Slug').fill('beta');
    const create = page.getByRole('button', { name: 'Create account' });
    const adopt = page.getByLabel('Adopt this slug');
    await create.click();
    await expect(page.locator('.form-alert')).toContainText('already exists');
    await expect(adopt).toHaveCount(0);
    await create.click();
    await expect(adopt).toBeVisible();
    await adopt.check();
    await create.click();
    await expect.poll(() => sent.length).toBe(3);
    expect((sent[2]!.body as T.CreateAccountRequest).adopt).toBe(true);
    expect((sent[0]!.body as T.CreateAccountRequest).adopt).toBeUndefined();
  });

  test('lists the instance settings read-only with their sources', async ({ page }) => {
    await setup(page, operatorMe, [[/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    await page.goto('/#/operator');
    const panel = page.locator('#op-instance').locator('../..');
    const row = panel.getByRole('row').filter({ hasText: g.instanceSetting.key });
    await expect(row).toContainText(g.instanceSetting.value);
    await expect(row).toContainText('config file');
    await expect(panel.locator('input, select, textarea')).toHaveCount(0);
  });

  test('shows why a file account is left out, beside the dashboard account holding its slug', async ({ page }) => {
    const conflict = `dashboard account "${S}" already holds the slug`;
    await setup(page, operatorMe, [
      [/\/api\/v1\/operator\/audit$/, g.pageOf([])],
      [/\/api\/v1\/operator\/accounts$/, [{ ...g.operatorAccount, live: true }, { ...g.operatorAccount, managedBy: 'file', live: false, revision: 0, conflict }]],
    ]);
    await page.goto('/#/operator');
    const rows = page.getByRole('row').filter({ hasText: S });
    await expect(rows).toHaveCount(2);
    await expect(rows.filter({ hasText: conflict })).toHaveCount(1);
    await expect(page.getByRole('button', { name: `Delete account ${S}` })).toHaveCount(1);
  });

  test('deletes an account after the slug is typed, reloading the revision on a conflict', async ({ page }) => {
    let lists = 0;
    await setup(page, operatorMe, [
      [/\/api\/v1\/operator\/audit$/, g.pageOf([g.auditEvent])],
      [/\/api\/v1\/operator\/accounts$/, () => [lists++ === 0 ? g.operatorAccount : { ...g.operatorAccount, revision: 5 }]],
    ]);
    const sent = await g.mockWrites(page, [
      ['DELETE', new RegExp(`/api/v1/accounts/${S}$`), () => (sent.length === 1 ? g.apiError(409, 'revision_conflict', 'changed') : { status: 204 })],
    ]);
    await page.goto('/#/operator');
    await expect(page.locator('#op-audit').locator('../..')).toContainText(g.auditEvent.action);
    await page.getByRole('button', { name: `Delete account ${S}` }).click();
    const dialog = page.getByRole('dialog');
    const confirm = dialog.getByRole('button', { name: 'Delete account' });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel('Type the slug to confirm').fill('wrong');
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel('Type the slug to confirm').fill(S);
    await confirm.click();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]!.url.searchParams.get('revision')).toBe(String(g.operatorAccount.revision));
    await expect(dialog.getByRole('alert')).toContainText('confirm again');
    await confirm.click();
    await expect.poll(() => sent.length).toBe(2);
    expect(sent[1]!.url.searchParams.get('revision')).toBe('5');
    await expect(page.getByRole('status')).toContainText(`Deleted account ${S}`);
  });
});
