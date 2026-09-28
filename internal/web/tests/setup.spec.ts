import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import * as g from './golden';
import type * as T from '../src/lib/types';

const operatorMe: T.Me = { ...g.me, operator: true };

const fresh: T.SetupStatus = {
  ...g.setupStatus,
  fileConnections: [],
  connections: [],
  reviewModel: '',
  embedding: false,
};

async function setup(page: Page, status: T.SetupStatus | (() => T.SetupStatus), rows: [RegExp, unknown][] = []): Promise<void> {
  await g.mockApi(page, [[/\/api\/v1\/me$/, operatorMe], [/\/api\/v1\/operator\/setup$/, status], ...rows, ...g.defaultApi()]);
}

// flags presets what the wizard remembers in this browser.
async function flags(page: Page, f: Record<string, boolean>): Promise<void> {
  await page.addInitScript((v) => localStorage.setItem('kritik.setup', v), JSON.stringify(f));
}

test.describe('setup wizard', () => {
  test('opens on a fresh instance, and once closed leaves a banner to resume it', async ({ page }) => {
    await setup(page, fresh);
    await page.goto('/#/');
    const wizard = page.getByRole('dialog', { name: 'Set up kritik' });
    await expect(wizard).toBeVisible();
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('Listener');
    await expect(wizard).toContainText(`${g.setupStatus.hooksUrl}<connection>`);
    await wizard.getByRole('button', { name: 'Close' }).click();
    await expect(wizard).toBeHidden();

    const banner = page.getByRole('note').filter({ hasText: 'cannot review yet' });
    await expect(banner).toContainText('no GitHub App is connected');
    await page.reload();
    await expect(banner).toBeVisible();
    await expect(wizard).toBeHidden();

    await banner.getByRole('button', { name: 'Resume setup' }).click();
    await wizard.getByRole('button', { name: 'Next' }).click();
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('GitHub App');
    await expect(wizard.getByRole('button', { name: 'Create on GitHub' })).toBeVisible();
    await expect(wizard.getByRole('button', { name: 'Next' })).toBeDisabled();
    await page.reload();
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('GitHub App');
  });

  test('saves the model key and review model, then registers the repositories', async ({ page }) => {
    let saved = false;
    await flags(page, { listener: true, installed: true });
    await setup(page, () => ({ ...fresh, connections: ['alpha-bot'], reviewModel: saved ? 'or/acme-large' : '' }), [
      [/\/api\/v1\/operator\/connections\/alpha-bot\/repositories$/, [g.golden<T.AccountRepositories>('account_repositories')]],
    ]);
    const sent = await g.mockWrites(page, [
      ['POST', /\/api\/v1\/operator\/providers\/test$/, { status: 200, body: g.testResult }],
      [
        'PUT',
        /\/api\/v1\/config$/,
        () => {
          saved = true;
          return { status: 200, body: { revision: 4 } };
        },
      ],
      ['POST', /\/api\/v1\/operator\/connections\/alpha-bot\/repositories$/, { status: 200, body: { added: 1 } }],
    ]);
    await page.goto('/#/');
    const wizard = page.getByRole('dialog', { name: 'Set up kritik' });
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('Model provider');
    await wizard.locator('[data-path="providers..name"]').fill('or');
    await wizard.getByLabel('API key: new value').fill('sk');
    await wizard.getByRole('button', { name: 'Test key' }).click();
    await expect(wizard.getByTestId('key-test-result')).toContainText('2 models offered');
    await expect(wizard.locator('#setup-models option')).toHaveCount(2);
    await wizard.getByLabel('Review model').fill('or/acme-large');
    await wizard.getByRole('button', { name: 'Save and continue' }).click();

    await expect(wizard.locator('[aria-current="step"]')).toHaveText('Embeddings');
    const put = sent.find((s) => s.method === 'PUT')!.body as T.UpdateConfigRequest;
    expect(put.revision).toBe(g.instanceConfig.revision);
    expect(put.spec.providers).toEqual({ or: { type: 'openrouter', apiKey: { value: 'sk' } } });
    expect(put.spec.defaults).toEqual({ models: { review: 'or/acme-large' } });
    const conn = (put.spec.connections as Record<string, unknown>[])[0]!;
    expect((conn.app as Record<string, unknown>).privateKey).toEqual({ keep: true });

    await wizard.getByRole('button', { name: 'Skip' }).click();
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('Repositories');
    await expect(wizard).toContainText('1 repository');
    await wizard.getByRole('button', { name: 'Register and continue' }).click();
    await expect(page.getByRole('status').filter({ hasText: 'Registered 1 new repository' })).toBeVisible();
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('Done');
    await wizard.getByRole('button', { name: 'Finish' }).click();
    await expect(wizard).toBeHidden();
    await expect(page.getByRole('note').filter({ hasText: 'cannot review yet' })).toHaveCount(0);
  });

  test('waits on the App being installed, or the admin saying so', async ({ page }) => {
    await flags(page, { listener: true });
    let installed = false;
    const conn = g.golden<T.AccountDetail>('account_detail').connection;
    await setup(page, { ...fresh, connections: [conn.name] }, [
      [
        new RegExp(`/api/v1/operator/connections/${conn.name}/installations$`),
        () => (installed ? [{ ...g.appInstallation, account: conn.accounts[0]!, served: true }] : []),
      ],
    ]);
    await page.goto('/#/');
    const wizard = page.getByRole('dialog', { name: 'Set up kritik' });
    await expect(wizard.locator('[aria-current="step"]')).toHaveText('GitHub App');
    await expect(wizard).toContainText('not installed yet');
    await expect(wizard.getByRole('button', { name: 'Next' })).toBeDisabled();
    installed = true;
    await expect(wizard.getByRole('button', { name: 'Next' })).toBeEnabled({ timeout: 10_000 });
    await expect(wizard.getByRole('listitem').filter({ hasText: conn.accounts[0]! })).toContainText('installed');
  });

  test('shows a finished App registration over the wizard', async ({ page }) => {
    await setup(page, fresh);
    await g.mockWrites(page, [['POST', /\/api\/v1\/app\/manifests\/collect$/, { status: 200, body: [g.appManifestResult] }]]);
    await page.goto('/#/operator');
    const results = page.getByRole('dialog', { name: 'GitHub App registration' });
    await expect(results).toBeVisible();
    await results.getByRole('button', { name: 'Done' }).click();
    await expect(results).toBeHidden();
    await expect(page.getByRole('dialog', { name: 'Set up kritik' })).toBeVisible();
  });

  test('stays away from an instance that can review', async ({ page }) => {
    await setup(page, g.setupStatus);
    await page.goto('/#/');
    await expect(page.locator('.page-head h1')).toBeVisible();
    await expect(page.getByRole('dialog', { name: 'Set up kritik' })).toBeHidden();
    await expect(page.getByRole('note').filter({ hasText: 'cannot review yet' })).toHaveCount(0);
  });
});
