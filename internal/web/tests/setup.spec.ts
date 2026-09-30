import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import * as g from './golden';
import type * as T from '../src/lib/types';

const adminMe: T.Me = { ...g.me, admin: true };

async function setup(page: Page, who: T.Me, status: T.SetupStatus): Promise<void> {
  await g.mockApi(page, [[/\/api\/v1\/me$/, who], [/\/api\/v1\/admin\/setup$/, status], ...g.defaultApi()]);
}

test.describe('setup banner', () => {
  test('tells an admin the instance cannot review yet, and leads to the checklist', async ({ page }) => {
    await setup(page, adminMe, { ...g.setupStatus, reviewModel: '' });
    await page.goto('/#/');
    const banner = page.getByRole('note').filter({ hasText: 'cannot review yet' });
    await expect(banner).toContainText('no review model is set');
    await banner.getByRole('link', { name: 'See what is missing' }).click();
    await expect(page).toHaveURL(/#\/admin$/);
    await expect(page.locator('#op-setup')).toBeVisible();
  });

  test('stays away from an instance that can review, and from a member', async ({ page }) => {
    await setup(page, adminMe, g.setupStatus);
    await page.goto('/#/');
    await expect(page.locator('.page-head h1')).toBeVisible();
    await expect(page.getByRole('note').filter({ hasText: 'cannot review yet' })).toHaveCount(0);

    await setup(page, { ...g.me, admin: false, accounts: [g.SLUG] }, { ...g.setupStatus, connections: [] });
    await page.reload();
    await expect(page.locator('.page-head h1')).toBeVisible();
    await expect(page.getByRole('note').filter({ hasText: 'cannot review yet' })).toHaveCount(0);
  });
});
