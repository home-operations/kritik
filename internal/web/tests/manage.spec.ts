import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import * as g from './golden';
import type * as T from '../src/lib/types';

const S = g.SLUG;
const API = `/api/v1/accounts/${S}`;
const ADMIN = `#/a/${S}/admin`;
const CONFIG = /\/api\/v1\/config$/;

const adminMe: T.Me = { ...g.me, operator: false, accounts: [{ slug: S, role: 'admin' }] };
const memberMe: T.Me = { ...g.me, operator: false, accounts: [{ slug: S, role: 'member' }] };
const operatorMe: T.Me = { ...g.me, operator: true };

// The golden account entry, with enough of it to exercise every part of
// the form.
const accountConfig: T.AccountConfig = {
  ...g.accountConfig,
  spec: {
    ...g.accountConfig.spec,
    limits: { concurrency: 2 },
    repositories: [{ name: 'one', mode: 'agentic', agent: { maxSteps: 10 }, konflate: 'keep-me' }],
  },
};
const ownKey = { type: 'openai', apiKey: { keep: true } };

// The golden instance spec, with settings its form has no control for.
const accountEntry = { forge: 'github', name: 'alpha', providers: { own: { type: 'openai', apiKey: { set: true } } } };
const instanceConfig: T.InstanceConfig = {
  ...g.instanceConfig,
  spec: { ...g.instanceConfig.spec, defaults: { settle: '1m' }, accounts: [accountEntry] },
};
const alphaBot = (g.instanceConfig.spec.connections as Record<string, unknown>[])[0]!;
const embedded: T.InstanceConfig = {
  ...instanceConfig,
  spec: { ...instanceConfig.spec, embedding: { baseUrl: 'https://embed.example/v1', model: 'm', dims: 8, apiKey: { set: true } } },
};

async function setup(page: Page, who: T.Me, rows: [RegExp, unknown][] = []): Promise<URL[]> {
  return g.mockApi(page, [[/\/api\/v1\/me$/, who], ...rows, ...g.defaultApi()]);
}

function accountRow(cfg: T.AccountConfig | ((u: URL) => T.AccountConfig)): [RegExp, unknown] {
  return [new RegExp(`${API}/config$`), cfg];
}

function instanceRow(cfg: T.InstanceConfig | ((u: URL) => T.InstanceConfig)): [RegExp, unknown] {
  return [CONFIG, cfg];
}

test.describe('account configuration', () => {
  test('an entry saves under the instance revision, carrying what the form has no control for', async ({ page }) => {
    let reads = 0;
    const seen = await setup(page, adminMe, [accountRow(() => (reads++ === 0 ? accountConfig : { ...accountConfig, revision: 4 }))]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('group', { name: `Account ${S}` })).toBeVisible();
    await page.locator('[data-path="limits.concurrency"]').fill('3');
    await page.getByRole('button', { name: 'Save' }).click();

    await expect.poll(() => sent.length).toBe(1);
    const body = sent[0]!.body as T.UpdateConfigRequest;
    expect(body.revision).toBe(3);
    expect(body.spec).toEqual({
      forge: 'github',
      name: 'alpha',
      providers: { own: ownKey },
      limits: { concurrency: 3 },
      repositories: [{ name: 'one', mode: 'agentic', agent: { maxSteps: 10 }, konflate: 'keep-me' }],
    });
    await expect(page.locator('#admin-config').locator('..')).toContainText('revision 4');
    expect(seen.filter((u) => u.pathname.endsWith('/config')).length).toBeGreaterThanOrEqual(2);
  });

  test('a configuration the caller cannot change renders read-only with secrets as set/not set', async ({ page }) => {
    await setup(page, adminMe, [accountRow({ ...accountConfig, editable: false })]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('note')).toContainText('not change it');
    await expect(page.locator('.spec-view')).toContainText('own');
    await expect(page.locator('.spec-view .pill').first()).toHaveText('set');
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
  });

  test('fields left empty show what they inherit, and from where', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig)]);
    await page.goto(`/${ADMIN}/config`);
    const inh = g.accountConfig.inherited;
    await expect(page.locator('[data-path="models.review"]')).toHaveAttribute('placeholder', `inherits ${inh.account.models.review} from the defaults`);
    await expect(page.locator('[data-path="settle"]')).toHaveAttribute('placeholder', "inherits 30s from kritik's default");
    await expect(page.locator('[data-path="repositories[0].mode"] option[value=""]')).toHaveText(`default: ${inh.repository.mode}`);
    await expect(page.locator('[data-path="repositories[0].filter"]')).toHaveAttribute('placeholder', `inherits ${inh.repository.filter} from kritik's default`);
  });

  test('a 422 highlights and focuses the field its path names', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig)]);
    const model = 'models.review';
    const key = 'providers.own.apiKey';
    const sent = await g.mockWrites(page, [
      [
        'PUT',
        new RegExp(`${API}/config$`),
        () =>
          sent.length <= 2
            ? g.apiError(422, 'invalid_spec', `${model}: no provider serves this model`, { path: model })
            : g.apiError(422, 'reenter_secret', 'enter this secret again', { path: key }),
      ],
    ]);
    await page.goto(`/${ADMIN}/config`);
    await page.locator(`[data-path="${model}"]`).fill('nowhere/model');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('no provider serves this model');
    await expect(page.locator(`[data-path="${model}"]`)).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator(`[data-path="${model}"]`)).toBeFocused();
    // The same error again still moves focus back to the field.
    await page.getByLabel('Filter').first().focus();
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(2);
    await expect(page.locator(`[data-path="${model}"]`)).toBeFocused();

    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('must be entered again');
    await expect(page.locator(`[data-path="${key}"]`)).toHaveClass(/invalid/);
    await expect(page.locator(`[data-path="${model}"]`)).not.toHaveAttribute('aria-invalid', 'true');
    // Adding or removing an item shifts indexes, so it dismisses a path error.
    await page.getByRole('button', { name: 'Add repository' }).click();
    await expect(page.locator(`[data-path="${key}"]`)).not.toHaveClass(/invalid/);
    await expect(page.locator('.form-alert')).toHaveCount(0);
  });

  test('an admin adds a provider key and sets the review model on it', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByRole('button', { name: 'Add provider key' }).click();
    await page.locator('[data-path="providers..name"]').fill('mine');
    await page.locator('[data-path="providers.mine.type"]').selectOption('anthropic');
    await page.locator('[data-path="providers.mine.apiKey"]').getByLabel('API key: new value').fill('sk-test');
    await page.locator('[data-path="models.review"]').fill('mine/claude');
    await page.getByRole('button', { name: 'Save' }).click();

    await expect.poll(() => sent.length).toBe(1);
    const spec = (sent[0]!.body as T.UpdateConfigRequest).spec;
    expect(spec.providers).toEqual({ own: ownKey, mine: { type: 'anthropic', apiKey: { value: 'sk-test' } } });
    expect((spec.models as Record<string, unknown>).review).toBe('mine/claude');
  });

  test('a provider key is not kept once its endpoint changes', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig)]);
    await page.goto(`/${ADMIN}/config`);
    const key = page.locator('[data-path="providers.own.apiKey"]');
    await expect(key.getByLabel('Keep current')).toBeChecked();
    await page.locator('[data-path="providers.own.baseUrl"]').fill('https://llm.example/v1');
    await expect(key.getByLabel('Keep current')).toHaveCount(0);
    await expect(page.getByRole('note').filter({ hasText: 'cannot be kept' })).toBeVisible();
  });

  test('switching to JSON with a typed secret is refused, so the typed value is still what saves', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    const key = page.locator('[data-path="providers.own.apiKey"]');
    await key.getByLabel('Replace with a new value').check();
    await key.getByLabel('API key: new value').fill('typed');
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    await expect(page.locator('.form-alert')).toContainText('JSON view never shows them');
    await expect(page.getByLabel('Spec JSON')).toHaveCount(0);
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const providers = (sent[0]!.body as T.UpdateConfigRequest).spec.providers as Record<string, Record<string, unknown>>;
    expect(providers.own!.apiKey).toEqual({ value: 'typed' });
  });

  test('a revision conflict offers to reload the latest', async ({ page }) => {
    let reads = 0;
    await setup(page, adminMe, [accountRow(() => (reads++ === 0 ? accountConfig : { ...accountConfig, revision: 9 }))]);
    await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), g.apiError(409, 'revision_conflict', 'the configuration was changed')]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByLabel('Filter').first().fill('changed');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('Someone else saved the configuration');
    await page.getByRole('button', { name: /Reload the latest/ }).click();
    await expect(page.locator('#admin-config').locator('..')).toContainText('revision 9');
    await expect(page.getByLabel('Filter').first()).toHaveValue('');
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('the JSON editor shows secrets as keep and saves the JSON as written', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    await page.locator('[data-path="providers.own.apiKey"]').getByLabel('Replace with a new value').check();
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    const box = page.getByLabel('Spec JSON');
    const spec = JSON.parse(await box.inputValue()) as Record<string, unknown>;
    expect(spec.providers).toEqual({ own: ownKey });

    await box.fill('{ not json');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.locator('.form-alert')).toContainText('does not parse');
    expect(sent).toHaveLength(0);

    await box.fill(JSON.stringify({ ...spec, filter: 'from-json' }));
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect((sent[0]!.body as T.UpdateConfigRequest).spec.filter).toBe('from-json');
  });

  test('unsaved edits ask before leaving', async ({ page }) => {
    await setup(page, adminMe, [accountRow(accountConfig), [new RegExp(`${API}/audit$`), g.pageOf([])]]);
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

  test('management disabled makes the account and instance configuration read-only', async ({ page }) => {
    await setup(page, operatorMe, [[/\/api\/v1\/meta$/, { ...g.meta, management: false }], accountRow(accountConfig)]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('note')).toContainText('no sealing key configured');
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
    await page.goto('/#/operator');
    await expect(page.getByRole('note')).toContainText('no sealing key configured');
    await expect(page.locator('.spec-view')).toContainText('alpha-bot');
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Add connection' })).toHaveCount(0);
    await expect(page.locator('#op-app')).toHaveCount(0);
  });

  test('an account member cannot open the admin page', async ({ page }) => {
    await setup(page, memberMe);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('alert')).toContainText('Only an admin');
    await expect(page.getByRole('link', { name: 'Admin' })).toHaveCount(0);
  });
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
  test('keep, replace and generate secret controls shape the PUT, and the generated secret shows once', async ({ page }) => {
    let reads = 0;
    const seen = await setup(page, operatorMe, [instanceRow(() => (reads++ === 0 ? instanceConfig : { ...instanceConfig, revision: 4 }))]);
    const sent = await g.mockWrites(page, [['PUT', CONFIG, { status: 200, body: g.configWriteResult }]]);
    await page.goto('/#/operator');

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
    const body = sent[0]!.body as T.UpdateConfigRequest;
    expect(body.revision).toBe(3);
    expect(body.spec).toEqual({
      connections: [
        { name: 'alpha-bot', forge: 'github', accounts: ['alpha'], app: { clientId: 'Iv1.alpha', privateKey: { value: 'key-new' }, webhookSecret: { generate: true } } },
      ],
      defaults: { settle: '1m' },
      accounts: [{ ...accountEntry, providers: { own: ownKey } }],
    });

    const dialog = page.getByRole('dialog', { name: 'Generated webhook secrets' });
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText('only time');
    await expect(dialog.getByTestId('generated-secret')).toHaveText('00ff');
    await expect(dialog).toContainText('/hooks/alpha-bot');
    await dialog.getByRole('button', { name: 'I have copied them' }).click();
    await expect(dialog).toBeHidden();
    await expect(page.getByTestId('generated-secret')).toHaveCount(0);
    // The Save button that opened it was remounted away; focus lands on the panel heading.
    await expect(page.locator('#op-config')).toBeFocused();
    // Saved: the spec reloads and the typed secret is gone with the old draft.
    await expect(page.locator('#op-config').locator('..')).toContainText('revision 4');
    await expect(page.locator('[data-path="connections[0].app.privateKey"]').getByLabel('Keep current')).toBeChecked();
    await expect(page.locator('input[type=password]')).toHaveCount(0);
    // A save changes which accounts and connections run, so their lists reload too.
    await expect.poll(() => seen.filter((u) => u.pathname.endsWith('/operator/accounts')).length).toBeGreaterThanOrEqual(2);
    await expect.poll(() => seen.filter((u) => u.pathname.endsWith('/operator/connections')).length).toBeGreaterThanOrEqual(2);
  });

  test('adds a connection to a fresh instance', async ({ page }) => {
    await setup(page, operatorMe, [instanceRow({ revision: 0, editable: true, spec: {} })]);
    const sent = await g.mockWrites(page, [
      ['PUT', CONFIG, { status: 200, body: { revision: 1, generated: { 'connections[beta-bot].app.webhookSecret': 'abcd' } } }],
    ]);
    await page.goto('/#/operator');
    await expect(page.getByText('No connections in the dashboard.')).toBeVisible();
    await page.getByRole('button', { name: 'Add connection' }).click();
    await page.getByLabel('Name', { exact: true }).fill('beta-bot');
    await expect(page.getByLabel('Forge')).toHaveValue('github');
    for (const other of ['github-enterprise', 'gitlab', 'forgejo', 'gitea']) {
      await expect(page.getByLabel('Forge').locator(`option[value="${other}"]`)).toHaveJSProperty('disabled', true);
    }
    await page.locator('[data-path="connections[0].accounts"]').fill('org-1\n  user-1 \n\n');
    await page.getByLabel('App client ID', { exact: true }).fill('Iv1.beta');
    await page.getByLabel('App private key: new value').fill('key');
    await page.getByRole('button', { name: 'Save' }).click();

    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]!.body).toEqual({
      revision: 0,
      spec: {
        connections: [
          {
            name: 'beta-bot',
            forge: 'github',
            accounts: ['org-1', 'user-1'],
            app: { clientId: 'Iv1.beta', privateKey: { value: 'key' }, webhookSecret: { generate: true } },
          },
        ],
      },
    });
    await expect(page.getByRole('dialog', { name: 'Generated webhook secrets' })).toContainText('/hooks/beta-bot');
  });

  test('a 422 highlights the connection field its path names', async ({ page }) => {
    await setup(page, operatorMe, [instanceRow(instanceConfig), [/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    const accounts = 'connections[0].accounts';
    const sent = await g.mockWrites(page, [
      [
        'PUT',
        CONFIG,
        () =>
          sent.length === 1
            ? g.apiError(422, 'invalid_spec', `${accounts}[1]: "org-2" is served by connection "file-bot" of the configuration file`, {
                path: `${accounts}[1]`,
              })
            : g.apiError(422, 'reenter_secret', 'enter this secret again', { path: 'connections[0].app.privateKey' }),
      ],
    ]);
    await page.goto('/#/operator');
    await page.locator(`[data-path="${accounts}"]`).fill('alpha\norg-2');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('is served by connection "file-bot"');
    await expect(page.locator(`[data-path="${accounts}"]`)).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator(`[data-path="${accounts}"]`)).toBeFocused();

    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('must be entered again');
    await expect(page.locator('[data-path="connections[0].app.privateKey"]')).toHaveClass(/invalid/);
    await expect(page.locator(`[data-path="${accounts}"]`)).not.toHaveAttribute('aria-invalid', 'true');
    await page.getByRole('button', { name: 'Add connection' }).click();
    await expect(page.locator('[data-path="connections[0].app.privateKey"]')).not.toHaveClass(/invalid/);
    await expect(page.locator('.form-alert')).toHaveCount(0);
  });

  test('a renamed connection cannot keep the secrets stored under its new name', async ({ page }) => {
    const two: T.InstanceConfig = { ...g.instanceConfig, spec: { connections: [alphaBot, { ...alphaBot, name: 'beta-bot', accounts: ['beta'] }] } };
    await setup(page, operatorMe, [instanceRow(two)]);
    const sent = await g.mockWrites(page, [['PUT', CONFIG, { status: 200, body: { revision: 4 } }]]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'Remove connection' }).first().click();
    await page.locator('[data-path="connections[0].name"]').fill('alpha-bot');
    await expect(page.getByRole('note').filter({ hasText: 'Renamed from' })).toContainText('beta-bot');
    const key = page.locator('[data-path="connections[0].app.privateKey"]');
    await expect(key.getByLabel('Keep current')).toHaveCount(0);
    await expect(page.locator('[data-path="connections[0].app.webhookSecret"]').getByLabel('Generate')).toBeChecked();
    await key.getByLabel('App private key: new value').fill('fresh');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const conns = (sent[0]!.body as T.UpdateConfigRequest).spec.connections as Record<string, unknown>[];
    expect(conns).toHaveLength(1);
    expect(conns[0]!.name).toBe('alpha-bot');
    const app = conns[0]!.app as Record<string, unknown>;
    expect(app.privateKey).toEqual({ value: 'fresh' });
    expect(app.webhookSecret).toEqual({ generate: true });
    expect(JSON.stringify(conns[0])).not.toContain('keep');
  });

  test('the JSON editor holds the whole spec, secrets as keep, and saves it as written', async ({ page }) => {
    await setup(page, operatorMe, [instanceRow(instanceConfig)]);
    const sent = await g.mockWrites(page, [['PUT', CONFIG, { status: 200, body: { revision: 4 } }]]);
    await page.goto('/#/operator');
    const key = page.locator('[data-path="connections[0].app.privateKey"]');
    await key.getByLabel('Replace with a new value').check();
    await key.getByLabel('App private key: new value').fill('typed');
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    await expect(page.locator('.form-alert')).toContainText('JSON view never shows them');
    await expect(page.getByLabel('Spec JSON')).toHaveCount(0);
    await key.getByLabel('App private key: new value').fill('');

    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    const box = page.getByLabel('Spec JSON');
    const spec = JSON.parse(await box.inputValue()) as Record<string, unknown>;
    const conn = (spec.connections as Record<string, unknown>[])[0]!;
    expect((conn.app as Record<string, unknown>).privateKey).toEqual({ keep: true });
    expect(spec.accounts).toEqual([{ ...accountEntry, providers: { own: ownKey } }]);

    await box.fill(JSON.stringify({ ...spec, polling: { interval: '10m' } }));
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const saved = (sent[0]!.body as T.UpdateConfigRequest).spec;
    expect(saved.polling).toEqual({ interval: '10m' });
    expect(saved.defaults).toEqual({ settle: '1m' });
  });

  test('a revision conflict offers to reload the latest instance spec', async ({ page }) => {
    let reads = 0;
    await setup(page, operatorMe, [
      instanceRow(() => (reads++ === 0 ? instanceConfig : { ...instanceConfig, revision: 9 })),
      [/\/api\/v1\/operator\/audit$/, g.pageOf([])],
    ]);
    await g.mockWrites(page, [['PUT', CONFIG, g.apiError(409, 'revision_conflict', 'the configuration was changed')]]);
    await page.goto('/#/operator');
    const accounts = page.locator('[data-path="connections[0].accounts"]');
    await accounts.fill('alpha\nbeta');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('Someone else saved the configuration');
    await page.getByRole('button', { name: /Reload the latest/ }).click();
    await expect(page.locator('#op-config').locator('..')).toContainText('revision 9');
    await expect(accounts).toHaveValue('alpha');
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('adds an embedder to the instance', async ({ page }) => {
    await setup(page, operatorMe, [instanceRow(instanceConfig)]);
    const sent = await g.mockWrites(page, [['PUT', CONFIG, { status: 200, body: { revision: 4 } }]]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'Add embedder' }).click();
    await page.getByLabel('Endpoint').fill('https://openrouter.ai/api/v1');
    await page.getByLabel('Model', { exact: true }).fill('voyage-code-3');
    await page.getByLabel('Dimension').fill('1024');
    await page.getByLabel('Embedding API key: new value').fill('ek');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const body = sent[0]!.body as T.UpdateConfigRequest;
    expect(body.confirmReindex).toBeUndefined();
    expect(body.spec.embedding).toEqual({ baseUrl: 'https://openrouter.ai/api/v1', model: 'voyage-code-3', dims: 1024, apiKey: { value: 'ek' } });
  });

  test('a new embedding model asks before rebuilding every index', async ({ page }) => {
    await setup(page, operatorMe, [instanceRow(embedded), [/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    const sent = await g.mockWrites(page, [
      [
        'PUT',
        CONFIG,
        (s) =>
          (s.body as T.UpdateConfigRequest).confirmReindex
            ? { status: 200, body: { revision: 4 } }
            : g.apiError(409, 'reindex_required', 'confirm the reindex to save', { path: 'embedding.model' }),
      ],
    ]);
    await page.goto('/#/operator');
    await expect(page.locator('[data-path="embedding.apiKey"]').getByLabel('Keep current')).toBeChecked();
    await page.locator('[data-path="embedding.model"]').fill('n');
    await page.getByRole('button', { name: 'Save' }).click();
    const dialog = page.getByRole('dialog', { name: 'Rebuild every index?' });
    await expect(dialog).toBeVisible();
    await dialog.getByRole('button', { name: 'Keep editing' }).click();
    await expect(dialog).toBeHidden();
    expect(sent).toHaveLength(1);
    await expect(page.locator('[data-path="embedding.model"]')).toHaveValue('n');
    await expect(page.getByRole('alert')).toHaveCount(0);

    await page.getByRole('button', { name: 'Save' }).click();
    await dialog.getByRole('button', { name: 'Save and reindex' }).click();
    await expect.poll(() => sent.length).toBe(3);
    const body = sent[2]!.body as T.UpdateConfigRequest;
    expect(body.confirmReindex).toBe(true);
    expect(body.spec.embedding).toEqual({ baseUrl: 'https://embed.example/v1', model: 'n', dims: 8, apiKey: { keep: true } });
    await expect(page.getByRole('status')).toContainText('Saved: revision 4');
  });

  test("the embedder's key is not kept once its endpoint changes", async ({ page }) => {
    await setup(page, operatorMe, [instanceRow(embedded)]);
    await page.goto('/#/operator');
    const key = page.locator('[data-path="embedding.apiKey"]');
    await page.locator('[data-path="embedding.baseUrl"]').fill('https://elsewhere.example/v1');
    await expect(key.getByLabel('Keep current')).toHaveCount(0);
    await expect(page.getByRole('note').filter({ hasText: 'endpoint changed' })).toBeVisible();
  });

  test('registers a GitHub App by posting its manifest to GitHub', async ({ page }) => {
    await setup(page, operatorMe, [[/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    const target = 'https://github.com/organizations/org-1/settings/apps/new?state=s1';
    const sent = await g.mockWrites(page, [
      ['POST', /\/api\/v1\/app\/manifests$/, { status: 200, body: { url: target, manifest: g.appManifestForm.manifest } }],
    ]);
    let posted = '';
    await page.route('https://github.com/**', async (route) => {
      posted = route.request().postData() ?? '';
      await route.fulfill({ status: 200, contentType: 'text/html', body: '<h1>GitHub</h1>' });
    });
    await page.goto('/#/operator');
    const panel = page.locator('#op-app').locator('../..');
    await panel.getByLabel('Connection name').fill('org-1-bot');
    await panel.getByLabel('An organization').check();
    await panel.getByLabel('Organization', { exact: true }).fill('org-1');
    await panel.getByLabel('Public: any account').check();
    await panel.getByRole('button', { name: 'Create on GitHub' }).click();
    await expect(page).toHaveURL(target);
    expect(sent[0]!.body).toEqual({ connection: 'org-1-bot', organization: 'org-1', public: true });
    expect(JSON.parse(new URLSearchParams(posted).get('manifest')!)).toEqual(g.appManifestForm.manifest);
  });

  test('a refused registration names the field', async ({ page }) => {
    await setup(page, operatorMe, [[/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    await g.mockWrites(page, [
      ['POST', /\/api\/v1\/app\/manifests$/, g.apiError(422, 'invalid_spec', 'a connection named alpha-bot already exists', { path: 'connection' })],
    ]);
    await page.goto('/#/operator');
    const panel = page.locator('#op-app').locator('../..');
    await panel.getByLabel('Connection name').fill('alpha-bot');
    await panel.getByRole('button', { name: 'Create on GitHub' }).click();
    await expect(panel.getByRole('alert')).toContainText('already exists');
    await expect(panel.getByLabel('Connection name')).toHaveAttribute('aria-invalid', 'true');
    await expect(page).toHaveURL(/#\/operator$/);
  });

  test("shows a registered App and its client secret once", async ({ page }) => {
    let collects = 0;
    await setup(page, operatorMe, [[/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    const failed: T.AppManifestResult = { connection: 'late-bot', error: "GitHub did not return the App's credentials: expired" };
    await g.mockWrites(page, [
      ['POST', /\/api\/v1\/app\/manifests\/collect$/, () => ({ status: 200, body: collects++ === 0 ? [g.appManifestResult, failed] : [] })],
    ]);
    await page.goto('/#/operator');
    const dialog = page.getByRole('dialog', { name: 'GitHub App registration' });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByTestId('app-client-secret')).toHaveText(g.appManifestResult.clientSecret!);
    await expect(dialog.getByTestId('app-client-id')).toHaveText(g.appManifestResult.clientId!);
    await expect(dialog.getByRole('link', { name: 'Install it on GitHub' })).toHaveAttribute('href', g.appManifestResult.installUrl!);
    await expect(dialog.getByRole('alert')).toContainText('did not return');
    await dialog.getByRole('button', { name: 'Done' }).click();
    await expect(dialog).toBeHidden();
    await expect(page.getByTestId('app-client-secret')).toHaveCount(0);
    await page.reload();
    await expect(page.locator('#op-app')).toBeVisible();
    await expect.poll(() => collects).toBe(2);
    await expect(dialog).toBeHidden();
  });

  test("lists an App's installations and uninstalls it from an account nobody serves", async ({ page }) => {
    const conn = g.golden<T.AccountDetail>('account_detail').connection;
    const path = `/api/v1/operator/connections/${conn.name}/installations`;
    const served: T.AppInstallation = { ...g.appInstallation, id: 1, account: 'alpha', accountType: 'Organization', allRepositories: true, served: true };
    let reads = 0;
    await setup(page, operatorMe, [
      [/\/api\/v1\/operator\/audit$/, g.pageOf([])],
      [new RegExp(`${path}$`), () => (reads++ === 0 ? [served, g.appInstallation] : [served])],
    ]);
    const sent = await g.mockWrites(page, [['DELETE', new RegExp(`${path}/${g.appInstallation.id}$`), { status: 204 }]]);
    await page.goto('/#/operator');
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

  test("tests a provider's key, and the embedder's, before saving", async ({ page }) => {
    await setup(page, operatorMe, [instanceRow(embedded), [/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    const sent = await g.mockWrites(page, [
      [
        'POST',
        /\/api\/v1\/operator\/providers\/test$/,
        (s) =>
          'value' in (s.body as T.ProviderTestRequest).apiKey
            ? { status: 200, body: { ok: false, error: 'POST "https://openrouter.ai/api/v1/key": 401 Unauthorized' } }
            : { status: 200, body: g.testResult },
      ],
      ['POST', /\/api\/v1\/operator\/embedding\/test$/, { status: 200, body: { ok: true } }],
    ]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'Add provider key' }).click();
    const prov = page.locator('.item-card').filter({ has: page.locator('[data-path="providers..name"]') });
    await prov.getByRole('button', { name: 'Test key' }).click();
    await expect(prov.getByTestId('key-test-result')).toHaveText('enter the key to test it');
    await prov.getByLabel('API key: new value').fill('sk-bad');
    await prov.getByRole('button', { name: 'Test key' }).click();
    await expect(prov.getByTestId('key-test-result')).toContainText('401 Unauthorized');
    expect(sent[0]!.body).toEqual({ type: 'openrouter', apiKey: { value: 'sk-bad' } });

    const emb = page.locator('.item-card').filter({ has: page.locator('[data-path="embedding.model"]') });
    await emb.getByRole('button', { name: 'Test key' }).click();
    await expect(emb.getByTestId('key-test-result')).toHaveText('The key works.');
    expect(sent[1]!.body).toEqual({ baseUrl: 'https://embed.example/v1', model: 'm', dims: 8, apiKey: { keep: true } });
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

  test('lists an account no connection serves without a link, and why', async ({ page }) => {
    await setup(page, operatorMe, [
      [/\/api\/v1\/operator\/audit$/, g.pageOf([])],
      [/\/api\/v1\/operator\/accounts$/, [{ ...g.operatorAccount, live: true, conflict: undefined }, { ...g.operatorAccount, slug: 'github/beta', connection: '' }]],
    ]);
    await page.goto('/#/operator');
    const live = page.getByRole('row').filter({ hasText: S });
    await expect(live).toHaveCount(1);
    await expect(live.getByRole('link', { name: S })).toBeVisible();
    await expect(live).toContainText(g.operatorAccount.connection);
    const unserved = page.getByRole('row').filter({ hasText: 'github/beta' });
    await expect(unserved).toContainText(g.operatorAccount.conflict!);
    await expect(unserved.getByRole('link')).toHaveCount(0);
  });
});

test.describe('settings search', () => {
  test('the palette finds a setting only when searching, and focuses it', async ({ page }) => {
    const adminOperator: T.Me = { ...g.me, operator: true, accounts: [{ slug: S, role: 'admin' }] };
    await setup(page, adminOperator, [accountRow(accountConfig), [/\/api\/v1\/operator\/audit$/, g.pageOf([])]]);
    await page.goto('/#/');
    await page.keyboard.press('ControlOrMeta+k');
    await expect(page.locator('.row-title').filter({ hasText: 'Tokens per month' })).toHaveCount(0);
    await page.keyboard.type('budget');
    await expect(page.locator('.palette-row')).toHaveCount(1);
    await expect(page.locator('.palette-row')).toContainText(`${S} settings`);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(new RegExp(`${ADMIN}/config$`));
    await expect(page.locator('[data-path="limits.tokensPerMonth"]')).toBeFocused();

    await page.keyboard.press('ControlOrMeta+k');
    await page.keyboard.type('embedder');
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/#\/operator$/);
    await expect(page.locator('#instance-embedding')).toBeFocused();
  });

  test("a repository's settings filter to what matches", async ({ page }) => {
    await setup(page, adminMe);
    await page.goto(`/#/a/${S}/repos/alpha/one`);
    const settings = page.locator('#repo-settings').locator('../..');
    await expect(settings.getByText('Review model')).toBeVisible();
    await settings.getByLabel('Filter settings').fill('settle');
    await expect(settings.locator('dt')).toHaveText(['Settle']);
    await expect(page.locator('#repo-agent').locator('../..').locator('dt')).toHaveCount(0);
    await settings.getByLabel('Filter settings').fill('agentic');
    await expect(settings.locator('dt')).toHaveText(['Mode']);
    await settings.getByLabel('Filter settings').fill('nothing-like-it');
    await expect(settings).toContainText('No setting matches');
  });
});
