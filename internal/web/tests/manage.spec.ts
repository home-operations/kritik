import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import * as g from './golden';
import type * as T from '../src/lib/types';

const S = g.SLUG;
const API = `/api/v1/tenants/${S}`;
const ADMIN = `#/t/${S}/admin`;

const adminMe: T.Me = { ...g.me, operator: false, tenants: [{ slug: S, role: 'admin', managedBy: 'dashboard' }] };
const memberMe: T.Me = { ...g.me, operator: false, tenants: [{ slug: S, role: 'member', managedBy: 'dashboard' }] };
const operatorMe: T.Me = { ...g.me, operator: true };

// The golden config, with enough spec to exercise every part of the form.
const inst0 = g.tenantConfig.spec.installations as Record<string, unknown>[];
const dashboardConfig: T.TenantConfig = {
  ...g.tenantConfig,
  spec: {
    ...g.tenantConfig.spec,
    limits: { concurrency: 2 },
    installations: [
      { ...inst0[0], forge: 'forgejo', host: 'https://code.example', account: 'bot', webhookSecret: { set: true } },
    ],
    repositories: [{ name: 'alpha/one', mode: 'agentic', agent: { maxSteps: 10 }, konflate: 'keep-me' }],
  },
};

async function setup(page: Page, who: T.Me, rows: [RegExp, unknown][] = []): Promise<URL[]> {
  return g.mockApi(page, [[/\/api\/v1\/me$/, who], ...rows, ...g.defaultApi()]);
}

function configRow(cfg: T.TenantConfig | ((u: URL) => T.TenantConfig)): [RegExp, unknown] {
  return [new RegExp(`${API}/config$`), cfg];
}

test.describe('tenant configuration', () => {
  test('a file tenant renders read-only with secrets as set/not set', async ({ page }) => {
    const file: T.TenantConfig = { ...dashboardConfig, managedBy: 'file', revision: null, editable: false };
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
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: g.tenantWriteResult }]]);
    await page.goto(`/${ADMIN}/config`);

    const token = page.locator('[data-path="installations[0].token"]');
    await expect(token.getByLabel('Keep current')).toBeChecked();
    // A password field is never pre-filled, including after switching away and back.
    await token.getByLabel('Replace with a new value').check();
    await token.getByLabel('Token: new value').fill('typed-then-dropped');
    await token.getByLabel('Keep current').check();
    await token.getByLabel('Replace with a new value').check();
    await expect(token.getByLabel('Token: new value')).toHaveValue('');
    await token.getByLabel('Token: new value').fill('tok-new');
    await page.locator('[data-path="installations[0].webhookSecret"]').getByLabel('Generate').check();

    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const body = sent[0]!.body as T.UpdateTenantRequest;
    expect(body.revision).toBe(3);
    const inst = (body.spec.installations as Record<string, unknown>[])[0]!;
    expect(inst.token).toEqual({ value: 'tok-new' });
    expect(inst.webhookSecret).toEqual({ generate: true });
    expect(inst).not.toHaveProperty('gitToken');
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
    await expect(page.locator('[data-path="installations[0].token"]').getByLabel('Keep current')).toBeChecked();
    await expect(page.locator('input[type=password]')).toHaveCount(0);
  });

  test('operator-only fields are disabled for a tenant admin and enabled for an operator', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByLabel('Concurrency')).toBeDisabled();
    await expect(page.locator('[data-path="repositories[0].mode"]')).toBeDisabled();
    await expect(page.locator('[data-path="repositories[0].agent"]')).toBeDisabled();
    await expect(page.locator('[data-path="runner"]')).toBeDisabled();
    await expect(page.locator('[data-path="models.review"]')).toBeDisabled();
    await expect(page.locator('[data-path="models.fallback"]')).toBeDisabled();
    await expect(page.locator('[data-path="forks"]')).toBeDisabled();
    await expect(page.getByText('(operator only)').first()).toBeVisible();

    await setup(page, operatorMe, [configRow(dashboardConfig)]);
    await page.reload();
    await expect(page.getByLabel('Concurrency')).toBeEnabled();
    await expect(page.locator('[data-path="repositories[0].mode"]')).toBeEnabled();
    await expect(page.locator('[data-path="models.review"]')).toBeEnabled();
    await expect(page.locator('[data-path="forks"]')).toBeEnabled();
  });

  test('a 422 highlights and focuses the field its path names', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const host = 'installations[0].host';
    const sent = await g.mockWrites(page, [
      [
        'PUT',
        new RegExp(`${API}/config$`),
        () =>
          sent.length <= 2
            ? g.apiError(422, 'invalid_spec', `${host}: the host is not allowed`, { path: host })
            : g.apiError(422, 'reenter_secret', 'enter this secret again', { path: 'installations[0].token' }),
      ],
    ]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByLabel('Host').fill('https://elsewhere.example');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('the host is not allowed');
    await expect(page.locator(`[data-path="${host}"]`)).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator(`[data-path="${host}"]`)).toBeFocused();
    // The same error again still moves focus back to the field.
    await page.getByLabel('Filter').first().focus();
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(2);
    await expect(page.locator(`[data-path="${host}"]`)).toBeFocused();

    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('must be entered again');
    await expect(page.locator('[data-path="installations[0].token"]')).toHaveClass(/invalid/);
    await expect(page.locator(`[data-path="${host}"]`)).not.toHaveAttribute('aria-invalid', 'true');
    // Adding or removing an item shifts indexes, so it dismisses a path error.
    await page.getByRole('button', { name: 'Add repository' }).click();
    await expect(page.locator('[data-path="installations[0].token"]')).not.toHaveClass(/invalid/);
    await expect(page.locator('.form-alert')).toHaveCount(0);
  });

  test('a renamed installation cannot keep the secrets stored under its new name', async ({ page }) => {
    const b = (dashboardConfig.spec.installations as Record<string, unknown>[])[0]!;
    const two: T.TenantConfig = {
      ...dashboardConfig,
      spec: { ...dashboardConfig.spec, installations: [b, { ...b, name: 'beta-bot' }] },
    };
    await setup(page, adminMe, [configRow(two)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByRole('button', { name: 'Remove installation' }).first().click();
    await page.locator('[data-path="installations[0].name"]').fill('alpha-bot');
    await expect(page.getByRole('note').filter({ hasText: 'Renamed from' })).toContainText('beta-bot');
    const token = page.locator('[data-path="installations[0].token"]');
    await expect(token.getByLabel('Keep current')).toHaveCount(0);
    await expect(page.locator('[data-path="installations[0].webhookSecret"]').getByLabel('Generate')).toBeChecked();
    await token.getByLabel('Token: new value').fill('fresh');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const insts = (sent[0]!.body as T.UpdateTenantRequest).spec.installations as Record<string, unknown>[];
    expect(insts).toHaveLength(1);
    expect(insts[0]!.name).toBe('alpha-bot');
    expect(insts[0]!.token).toEqual({ value: 'fresh' });
    expect(insts[0]!.webhookSecret).toEqual({ generate: true });
    expect(JSON.stringify(insts[0])).not.toContain('keep');
  });

  test('switching to JSON with a typed secret is refused, so the typed value is still what saves', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    const token = page.locator('[data-path="installations[0].token"]');
    await token.getByLabel('Replace with a new value').check();
    await token.getByLabel('Token: new value').fill('typed');
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    await expect(page.locator('.form-alert')).toContainText('JSON view never shows them');
    await expect(page.getByLabel('Spec JSON')).toHaveCount(0);
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    const inst = ((sent[0]!.body as T.UpdateTenantRequest).spec.installations as Record<string, unknown>[])[0]!;
    expect(inst.token).toEqual({ value: 'typed' });
  });

  test('a revision conflict offers to reload the latest', async ({ page }) => {
    let reads = 0;
    await setup(page, adminMe, [configRow(() => (reads++ === 0 ? dashboardConfig : { ...dashboardConfig, revision: 9 }))]);
    await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), g.apiError(409, 'revision_conflict', 'the tenant was changed')]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByLabel('Filter').first().fill('changed');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByRole('alert')).toContainText('Someone else saved this tenant');
    await page.getByRole('button', { name: /Reload the latest/ }).click();
    await expect(page.locator('#admin-config').locator('..')).toContainText('revision 9');
    await expect(page.getByLabel('Filter').first()).toHaveValue('');
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('the JSON editor shows secrets as keep and saves the JSON as written', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig)]);
    const sent = await g.mockWrites(page, [['PUT', new RegExp(`${API}/config$`), { status: 200, body: { slug: S, revision: 4 } }]]);
    await page.goto(`/${ADMIN}/config`);
    const token = page.locator('[data-path="installations[0].token"]');
    await token.getByLabel('Replace with a new value').check();
    await page.getByRole('button', { name: 'Advanced: edit JSON' }).click();
    const box = page.getByLabel('Spec JSON');
    const text = await box.inputValue();
    const spec = JSON.parse(text) as Record<string, unknown>;
    expect((spec.installations as Record<string, unknown>[])[0]!.token).toEqual({ keep: true });

    await box.fill('{ not json');
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.locator('.form-alert')).toContainText('does not parse');
    expect(sent).toHaveLength(0);

    await box.fill(JSON.stringify({ ...spec, filter: 'from-json' }));
    await page.getByRole('button', { name: 'Save' }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect((sent[0]!.body as T.UpdateTenantRequest).spec.filter).toBe('from-json');
  });

  test('unsaved edits ask before leaving', async ({ page }) => {
    await setup(page, adminMe, [configRow(dashboardConfig), [new RegExp(`${API}/members$`), g.members]]);
    await page.goto(`/${ADMIN}/config`);
    await page.getByLabel('Filter').first().fill('draft');
    page.once('dialog', (d) => void d.dismiss());
    await page.getByRole('link', { name: 'Members' }).click();
    await expect(page).toHaveURL(new RegExp(`${ADMIN}/config$`));
    await expect(page.getByLabel('Filter').first()).toHaveValue('draft');
    page.once('dialog', (d) => void d.accept());
    await page.getByRole('link', { name: 'Members' }).click();
    await expect(page).toHaveURL(new RegExp(`${ADMIN}/members$`));
  });

  test('management disabled makes the config read-only and hides tenant creation', async ({ page }) => {
    await setup(page, operatorMe, [[/\/api\/v1\/meta$/, { ...g.meta, management: false }], configRow(dashboardConfig)]);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('note')).toContainText('no sealing key configured');
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
    await page.goto('/#/operator');
    await expect(page.getByRole('note')).toContainText('no sealing key configured');
    await expect(page.getByRole('button', { name: 'New tenant' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: /Delete tenant/ })).toHaveCount(0);
  });

  test('a tenant member cannot open the admin page', async ({ page }) => {
    await setup(page, memberMe);
    await page.goto(`/${ADMIN}/config`);
    await expect(page.getByRole('alert')).toContainText('Only a tenant admin');
    await expect(page.getByRole('link', { name: 'Admin' })).toHaveCount(0);
  });
});

test.describe('members and invites', () => {
  const forgeOnly: T.Member = {
    account: { ...g.members.members[0]!.account, id: 'acct-2', displayName: 'Bea', email: 'bea@example.com' },
    role: 'member',
    sources: [{ source: 'forge', role: 'member' }],
  };
  const list: T.Members = { ...g.members, members: [...g.members.members, forgeOnly] };

  test('change an invite role, remove invite access, and surface last_admin', async ({ page }) => {
    await setup(page, adminMe, [[new RegExp(`${API}/members$`), list]]);
    const member = new RegExp(`${API}/members/acct-1$`);
    const sent = await g.mockWrites(page, [
      ['PATCH', member, () => (sent.length === 1 ? { status: 200, body: g.members.members[0] } : g.apiError(409, 'last_admin', 'no admin'))],
      ['DELETE', member, { status: 200, body: g.memberRemoved }],
    ]);
    await page.goto(`/${ADMIN}/members`);
    const rows = page.locator('#admin-members').locator('../..').locator('tbody tr');
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(1)).toContainText('from the forge at sign-in');
    await expect(rows.nth(1).getByRole('combobox')).toHaveCount(0);

    const role = page.getByLabel('Invite role for Ada');
    await role.selectOption('member');
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]!.body).toEqual({ role: 'member' });
    await expect(page.getByRole('status')).toContainText('Ada is now member');

    await role.selectOption('member');
    await expect(page.locator('.state-error')).toContainText('left with no admin');

    await page.getByRole('button', { name: 'Remove', exact: true }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Remove access' }).click();
    await expect.poll(() => sent.filter((s) => s.method === 'DELETE').length).toBe(1);
    await expect(page.getByRole('status')).toContainText(g.memberRemoved.note);
  });

  test('create and revoke invites', async ({ page }) => {
    await setup(page, adminMe, [[new RegExp(`${API}/members$`), g.members]]);
    const invite = g.members.invites![0]!;
    const sent = await g.mockWrites(page, [
      ['POST', new RegExp(`${API}/invites$`), { status: 201, body: { ...invite, id: 'inv-2', email: 'cy@example.com', role: 'admin' } }],
      ['DELETE', new RegExp(`${API}/invites/inv-1$`), { status: 204 }],
    ]);
    await page.goto(`/${ADMIN}/members`);
    await expect(page.locator('#admin-invites').locator('../..')).toContainText(invite.email);

    await page.getByLabel('Email').fill('cy@example.com');
    await page.locator('.inline-form').getByRole('combobox').selectOption('admin');
    await page.getByLabel(/Expires after/).fill('24');
    await page.getByRole('button', { name: 'Invite', exact: true }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]!.body).toEqual({ email: 'cy@example.com', role: 'admin', ttlHours: 24 });
    await expect(page.getByRole('status')).toContainText('Invited cy@example.com as admin');

    await page.getByRole('button', { name: `Revoke the invite for ${invite.email}` }).click();
    await expect.poll(() => sent.filter((s) => s.method === 'DELETE').length).toBe(1);
    await expect(page.getByRole('status')).toContainText(`Revoked the invite for ${invite.email}`);
  });
});

test('the audit log pages and expands detail', async ({ page }) => {
  const older: T.AuditEvent = { ...g.auditEvent, id: '6', action: 'invite.create', target: 'inv-1', detail: {} };
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

    await page.goto(`/#/t/${S}/reviews/rev-1`);
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

    await page.goto(`/#/t/${S}/pulls/alpha/one/7`);
    await page.getByRole('button', { name: 'Re-run' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Re-run' }).click();
    await expect.poll(() => sent.filter((s) => s.url.pathname.endsWith('/rerun')).length).toBe(2);
    await expect(page.getByRole('status')).toContainText('already queued or running');

    await page.goto(`/#/t/${S}/repos/alpha/one`);
    await page.getByRole('button', { name: 'Reindex' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Reindex' }).click();
    await expect(page.getByRole('status')).toContainText('cannot queue dashboard actions');
  });

  test('are hidden from a tenant member', async ({ page }) => {
    await setup(page, memberMe);
    await page.goto(`/#/t/${S}/reviews/rev-1`);
    await expect(page.locator('.page-head h1')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Re-run' })).toHaveCount(0);
    await page.goto(`/#/t/${S}/repos/alpha/one`);
    await expect(page.locator('#repo-settings')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Reindex' })).toHaveCount(0);
  });
});

test.describe('operator console', () => {
  test('creates a tenant', async ({ page }) => {
    await setup(page, operatorMe);
    const sent = await g.mockWrites(page, [
      ['POST', /\/api\/v1\/tenants$/, { status: 201, body: { ...g.tenantWriteResult, slug: 'beta', generated: { 'installations[beta-bot].webhookSecret': 'abcd' } } }],
    ]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'New tenant' }).click();
    await page.getByLabel('Slug').fill('beta');
    await page.getByRole('button', { name: 'Add installation' }).click();
    await page.getByLabel('Name', { exact: true }).fill('beta-bot');
    await page.getByLabel('Forge').selectOption('forgejo');
    await page.getByLabel('Host').fill('https://code.example');
    await page.locator('[data-path="installations[0].account"]').fill('bot');
    await page.getByLabel('Token: new value').fill('tok');
    await page.getByLabel('Concurrency').fill('3');
    await page.getByRole('button', { name: 'Create tenant' }).click();

    await expect.poll(() => sent.length).toBe(1);
    const body = sent[0]!.body as T.CreateTenantRequest;
    expect(body.slug).toBe('beta');
    expect(body.spec).toEqual({
      slug: 'beta',
      installations: [
        { name: 'beta-bot', forge: 'forgejo', host: 'https://code.example', account: 'bot', token: { value: 'tok' }, webhookSecret: { generate: true } },
      ],
      limits: { concurrency: 3 },
    });
    await expect(page.getByRole('dialog', { name: 'Generated webhook secrets' })).toContainText('/hooks/beta-bot');
  });

  test('offers adopt only for a slug a gone tenant used, and sends it', async ({ page }) => {
    await setup(page, operatorMe);
    const sent = await g.mockWrites(page, [
      [
        'POST',
        /\/api\/v1\/tenants$/,
        (s) => {
          const n = sent.length;
          if (n === 1) return g.apiError(409, 'slug_taken', 'a dashboard tenant with this slug already exists', { path: 'slug' });
          if (n === 2) return g.apiError(409, 'slug_taken', 'a tenant used this slug before', { path: 'slug', adoptable: true });
          return (s.body as T.CreateTenantRequest).adopt ? { status: 201, body: g.tenantWriteResult } : g.apiError(409, 'slug_taken', 'again', { path: 'slug', adoptable: true });
        },
      ],
    ]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'New tenant' }).click();
    await page.getByLabel('Slug').fill('beta');
    const create = page.getByRole('button', { name: 'Create tenant' });
    const adopt = page.getByLabel('Adopt this slug');
    await create.click();
    await expect(page.locator('.form-alert')).toContainText('already exists');
    await expect(adopt).toHaveCount(0);
    await create.click();
    await expect(adopt).toBeVisible();
    await adopt.check();
    await create.click();
    await expect.poll(() => sent.length).toBe(3);
    expect((sent[2]!.body as T.CreateTenantRequest).adopt).toBe(true);
    expect((sent[0]!.body as T.CreateTenantRequest).adopt).toBeUndefined();
  });

  test('refuses a plain-http installation host before sending', async ({ page }) => {
    await setup(page, operatorMe);
    const sent = await g.mockWrites(page, [['POST', /\/api\/v1\/tenants$/, { status: 201, body: g.tenantWriteResult }]]);
    await page.goto('/#/operator');
    await page.getByRole('button', { name: 'New tenant' }).click();
    await page.getByLabel('Slug').fill('beta');
    await page.getByRole('button', { name: 'Add installation' }).click();
    await page.getByLabel('Name', { exact: true }).fill('beta-bot');
    await page.getByLabel('Forge').selectOption('gitlab');
    await page.getByLabel('Host').fill('http://code.example');
    await page.locator('[data-path="installations[0].account"]').fill('bot');
    await page.getByLabel('Token: new value').fill('tok');
    await page.getByRole('button', { name: 'Create tenant' }).click();
    await expect(page.locator('.form-alert')).toContainText('https');
    await expect(page.locator('[data-path="installations[0].host"]')).toBeFocused();
    expect(sent).toHaveLength(0);
  });

  test('deletes a tenant after the slug is typed, reloading the revision on a conflict', async ({ page }) => {
    let lists = 0;
    await setup(page, operatorMe, [
      [/\/api\/v1\/operator\/audit$/, g.pageOf([g.auditEvent])],
      [/\/api\/v1\/operator\/tenants$/, () => [lists++ === 0 ? g.operatorTenant : { ...g.operatorTenant, revision: 5 }]],
    ]);
    const sent = await g.mockWrites(page, [
      ['DELETE', new RegExp(`/api/v1/tenants/${S}$`), () => (sent.length === 1 ? g.apiError(409, 'revision_conflict', 'changed') : { status: 204 })],
    ]);
    await page.goto('/#/operator');
    await expect(page.locator('#op-audit').locator('../..')).toContainText(g.auditEvent.action);
    await page.getByRole('button', { name: `Delete tenant ${S}` }).click();
    const dialog = page.getByRole('dialog');
    const confirm = dialog.getByRole('button', { name: 'Delete tenant' });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel('Type the slug to confirm').fill('wrong');
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel('Type the slug to confirm').fill(S);
    await confirm.click();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]!.url.searchParams.get('revision')).toBe(String(g.operatorTenant.revision));
    await expect(dialog.getByRole('alert')).toContainText('confirm again');
    await confirm.click();
    await expect.poll(() => sent.length).toBe(2);
    expect(sent[1]!.url.searchParams.get('revision')).toBe('5');
    await expect(page.getByRole('status')).toContainText(`Deleted tenant ${S}`);
  });
});
