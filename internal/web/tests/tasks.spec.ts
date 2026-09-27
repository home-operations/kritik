import { test, expect } from './fixtures';
import * as g from './golden';
import type * as T from '../src/lib/types';

const T_ = `#/t/${g.SLUG}`;
const api = `/api/v1/tenants/${g.SLUG}`;
const run = g.golden<T.TaskRun>('task_run');
const detail = g.golden<T.TaskRunDetail>('task_run_detail');

function taskApi(): [RegExp, unknown][] {
  return [
    [new RegExp(`${api}/task-runs$`), g.pageOf([run])],
    [new RegExp(`${api}/task-runs/${run.id}$`), detail],
    [new RegExp(`${api}/task-runs/${run.id}/transcript$`), g.transcript],
  ];
}

test('a repository links to its task runs, which filter by task and status', async ({ page }) => {
  const seen = await g.mockApi(page, [...taskApi(), ...g.defaultApi()]);
  await page.goto(`/${T_}/repos/alpha/one`);
  await page.getByRole('link', { name: 'Task runs', exact: true }).last().click();
  await expect(page).toHaveURL(new RegExp(`${T_}/repos/alpha/one/task-runs$`));
  const row = page.locator('table.task-runs tbody tr');
  await expect(row).toHaveCount(1);
  await expect(row).toContainText(run.status);
  await expect(row).toContainText(run.task);
  await expect(row).toContainText(`#${run.subjectNumber}`);
  await expect(row).toContainText(`${run.droppedCount} dropped`);
  // A repository's list has no repository column.
  await expect(page.locator('table.task-runs thead')).not.toContainText('Repository');
  const runs = () => seen.filter((u) => u.pathname.endsWith('/task-runs'));
  expect(runs()[0]!.searchParams.get('repo')).toBe('alpha/one');

  await page.getByLabel('Status').selectOption('failed');
  await page.getByPlaceholder('Task name').fill('triage');
  await expect
    .poll(() => runs().some((u) => u.searchParams.get('status') === 'failed' && u.searchParams.get('task') === 'triage'))
    .toBe(true);
});

test('a task run shows applied and dropped actions, fields, event and transcript', async ({ page }) => {
  await g.mockApi(page, [...taskApi(), ...g.defaultApi()]);
  await page.goto(`/${T_}/task-runs`);
  await expect(page.locator('table.task-runs thead')).toContainText('Repository');
  await page.getByRole('link', { name: run.task }).click();
  await expect(page).toHaveURL(new RegExp(`${T_}/task-runs/${run.id}$`));
  await expect(page.locator('h1')).toContainText(run.task);

  const applied = page.locator('tr.action-applied');
  const a = detail.applied!;
  await expect(applied).toHaveCount(a.addLabels.length + a.removeLabels.length + a.inline.length + 1);
  await expect(applied.filter({ hasText: 'labels.add' })).toContainText(a.addLabels[0]!);
  await expect(applied.filter({ hasText: 'labels.remove' })).toContainText(a.removeLabels[0]!);
  const c = a.inline[0]!;
  await expect(applied.filter({ hasText: 'inline' })).toContainText(`${c.path}:${c.line}-${c.endLine}`);
  await expect(applied.filter({ hasText: 'comment' })).toContainText(a.comment);
  const dropped = page.locator('tr.action-dropped');
  const d = detail.dropped[0]!;
  await expect(dropped).toHaveCount(detail.dropped.length);
  await expect(dropped).toContainText('dropped');
  await expect(dropped).toContainText(d.value);
  await expect(dropped).toContainText(d.reason);

  const fields = page.locator('#task-fields').locator('../..').locator('tbody tr');
  await expect(fields).toHaveCount(Object.keys(detail.fields).length);
  await expect(fields.first()).toContainText('priority');
  await expect(fields.first()).toContainText(String(detail.fields.priority));

  const event = page.locator('#task-event').locator('../..');
  await expect(event).toContainText(`${detail.event!.rawEvent}.${detail.event!.action}`);
  await expect(event).toContainText(detail.event!.sender);
  await expect(page.locator('#task-answer').locator('../..')).toContainText(detail.proposed!.summary);

  await page.getByRole('button', { name: 'Transcript' }).click();
  await expect(page.locator('.turn')).toHaveCount(g.transcript.turns.length);
});

test('a queued run whose event was swept says so', async ({ page }) => {
  const queued: T.TaskRunDetail = {
    ...detail,
    run: { ...run, status: 'queued', startedAt: null, finishedAt: null, durationMs: null, droppedCount: 0 },
    event: null,
    fields: {},
    proposed: null,
    applied: null,
    dropped: [],
  };
  await g.mockApi(page, [[new RegExp(`${api}/task-runs/${run.id}$`), queued], ...g.defaultApi()]);
  await page.goto(`/${T_}/task-runs/${run.id}`);
  await expect(page.locator('#task-actions').locator('../..')).toContainText('No actions recorded yet.');
  await expect(page.locator('#task-event').locator('../..')).toContainText('past its retention');
  await expect(page.locator('#task-fields').locator('../..')).toContainText('No fields.');
  await expect(page.locator('#task-answer')).toHaveCount(0);
});

test('a task run event refetches the list', async ({ page }) => {
  const seen = await g.mockApi(page, [...taskApi(), ...g.defaultApi()]);
  const ev: T.LiveEvent = { ...g.liveEvent, kind: 'task_run', id: run.id, reviewId: null };
  await page.route('**/api/events', (route) =>
    route.fulfill({ status: 200, contentType: 'text/event-stream', body: `event: task_run\ndata: ${JSON.stringify(ev)}\n\n` }),
  );
  await page.goto(`/${T_}/task-runs`);
  await expect.poll(() => seen.filter((u) => u.pathname.endsWith('/task-runs')).length).toBeGreaterThan(1);
});
