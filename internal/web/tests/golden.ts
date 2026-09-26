// Playwright fixtures built from the Go API's golden JSON files
// (internal/webapi/testdata/*.golden.json), which pin the wire format of
// every DTO. Reading them here instead of hand-writing mock bodies means a
// DTO change on the Go side shows up as a failing UI test, not a silently
// stale mock. Where a golden is too thin to exercise a view (one diff
// header, one of each list), tests extend it with spreads of the golden
// itself, typed against src/lib/types.ts.
import { readFileSync } from 'node:fs';
import type { Page as PWPage, Route } from '@playwright/test';
import type * as T from '../src/lib/types';

const dir = new URL('../../webapi/testdata/', import.meta.url);

export function golden<V>(name: string): V {
  return JSON.parse(readFileSync(new URL(`${name}.golden.json`, dir), 'utf8')) as V;
}

export const me = golden<T.Me>('me');
export const tenantSummary = golden<T.TenantSummary>('tenant_summary');
export const operatorTenant = golden<T.OperatorTenant>('operator_tenant');
export const repoPage = golden<T.Page<T.Repository>>('page');
export const repoDetail = golden<T.RepoDetail>('repo_detail');
export const pull = golden<T.Pull>('pull');
export const pullDetail = golden<T.PullDetail>('pull_detail');
export const reviewDetail = golden<T.ReviewDetail>('review_detail');
export const reviewDiff = golden<T.ReviewDiff>('review_diff');
export const reviewRaw = golden<T.ReviewRaw>('review_raw');
export const transcript = golden<T.Transcript>('transcript');
export const job = golden<T.Job>('job');
export const followup = golden<T.Followup>('followup');
export const usageSeries = golden<T.UsageSeries>('usage_series');
export const liveEvent = golden<T.LiveEvent>('event');
export const meta = golden<T.Meta>('meta');
export const tenantConfig = golden<T.TenantConfig>('tenant_config');
export const tenantWriteResult = golden<T.TenantWriteResult>('tenant_write_result');
export const members = golden<T.Members>('members');
export const memberRemoved = golden<T.MemberRemoved>('member_removed');
export const auditEvent = golden<T.AuditEvent>('audit_event');
export const accepted = golden<T.Accepted>('accepted');

export const SLUG = me.tenants[0]!.slug;

// The golden diff is only the file header; add a hunk whose new-side line
// 3 is where the golden finding (a.go:3) points.
export const diffWithHunk: T.ReviewDiff = {
  ...reviewDiff,
  diff: `${reviewDiff.diff}--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,4 @@\n package a\n \n+var x *int\n func f() {}\n`,
};

export function pageOf<V>(items: V[], nextCursor: string | null = null): T.Page<V> {
  return { items, nextCursor };
}

type Body = unknown | ((url: URL) => unknown);

// mockApi answers /api/v1/* from a table of [pathname pattern, body] rows
// (first match wins) and records every request URL so a test can assert on
// query parameters. Anything unmatched is a 404 with the API's error shape.
export async function mockApi(page: PWPage, table: [RegExp, Body][]): Promise<URL[]> {
  const seen: URL[] = [];
  await page.route('**/api/v1/**', (route: Route) => {
    const url = new URL(route.request().url());
    seen.push(url);
    const hit = table.find(([re]) => re.test(url.pathname));
    if (!hit) {
      return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify(golden('error')) });
    }
    const body = typeof hit[1] === 'function' ? (hit[1] as (u: URL) => unknown)(url) : hit[1];
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
  });
  return seen;
}

const t = `/api/v1/tenants/${SLUG}`;

// defaultApi is every read endpoint answered from its golden.
export function defaultApi(): [RegExp, Body][] {
  return [
    [/\/api\/v1\/meta$/, meta],
    [/\/api\/v1\/me$/, me],
    [/\/api\/v1\/tenants$/, [tenantSummary]],
    [/\/api\/v1\/operator\/tenants$/, [operatorTenant]],
    [new RegExp(`${t}/repos$`), repoPage],
    [new RegExp(`${t}/repos/alpha/one$`), repoDetail],
    [
      new RegExp(`${t}/pulls$`),
      (u: URL) => (u.searchParams.get('cursor') ? pageOf([{ ...pull, number: 8, title: 'More widgets', url: pull.url.replace(/\d+$/, '8') }]) : pageOf([pull], repoPage.nextCursor)),
    ],
    [new RegExp(`${t}/pulls/alpha/one/7$`), pullDetail],
    [new RegExp(`${t}/followups$`), pageOf([followup])],
    [new RegExp(`${t}/followups/\\d+/transcript$`), transcript],
    [new RegExp(`${t}/reviews/rev-1$`), reviewDetail],
    [new RegExp(`${t}/reviews/rev-1/diff$`), diffWithHunk],
    [new RegExp(`${t}/reviews/rev-1/transcript$`), transcript],
    [new RegExp(`${t}/reviews/rev-1/raw$`), reviewRaw],
    [new RegExp(`${t}/usage$`), usageSeries],
    [new RegExp(`${t}/queue$`), [job]],
    [new RegExp(`${t}$`), golden<T.TenantDetail>('tenant_detail')],
  ];
}

// A write the tests answer: status and body (none for 204).
export interface Reply {
  status: number;
  body?: unknown;
}

export interface Sent {
  method: string;
  url: URL;
  body: unknown;
}

// mockWrites answers non-GET /api/v1/* requests from [method, pathname
// pattern, reply] rows (first match wins) and records each with its JSON
// body. It takes precedence over mockApi, so register it after; GETs and
// unmatched writes fall through to mockApi.
export async function mockWrites(page: PWPage, rows: [string, RegExp, Reply | ((s: Sent) => Reply)][]): Promise<Sent[]> {
  const sent: Sent[] = [];
  await page.route('**/api/v1/**', (route: Route) => {
    const req = route.request();
    const url = new URL(req.url());
    const hit = rows.find(([m, re]) => m === req.method() && re.test(url.pathname));
    if (!hit) return route.fallback();
    const raw = req.postData();
    const s: Sent = { method: req.method(), url, body: raw ? (JSON.parse(raw) as unknown) : undefined };
    sent.push(s);
    const r = typeof hit[2] === 'function' ? hit[2](s) : hit[2];
    if (r.body === undefined) return route.fulfill({ status: r.status });
    return route.fulfill({ status: r.status, contentType: 'application/json', body: JSON.stringify(r.body) });
  });
  return sent;
}

export function apiError(status: number, code: string, message: string, details?: unknown): Reply {
  return { status, body: { code, message, details } };
}
