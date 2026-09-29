// Pure, rune-free route parsing/serialization for the hash router. Kept out
// of router.svelte.ts (which needs the .svelte.ts extension because of its
// `$state` rune) so these functions can be imported by tooling that doesn't
// go through Svelte's compiler -- e.g. a plain Playwright test.
//
//   #/                                        overview (account picker / landing)
//   #/signin                                  sign-in page
//   #/admin                                   admin console (cross-account)
//   #/a/<slug>                                account overview; slug is <forge>/<name>
//   #/a/<slug>/repos                          account's repo list
//   #/a/<slug>/repos/<owner>/<repo>           one repo
//   #/a/<slug>/pulls[?<filter>]               account's pull list; filter is a PullFilter
//   #/a/<slug>/pulls/<owner>/<repo>/<n>       one pull request
//   #/a/<slug>/reviews/<id>[/<tab>]           one review, optional tab
//   #/a/<slug>/findings[?<filter>]            account's findings; filter is a FindingFilter
//   #/a/<slug>/rules                          the files reviews read
//   #/a/<slug>/queue                          run queue
//   #/a/<slug>/usage                          usage/cost dashboard
//   #/a/<slug>/followups                      follow-up tracker
//   #/a/<slug>/admin[/<section>]              an account's settings: its configuration or audit log
//
// Segments round-trip through encodeURIComponent/decodeURIComponent, so a
// slug/owner/repo/id/section containing a literal "/" or other reserved
// character survives href() -> parse(). A single trailing slash is
// tolerated -- "#/a/<slug>/repos/" parses exactly like "#/a/<slug>/repos" --
// since href() never produces one but a bookmark or typed URL might. Any
// other malformation -- an empty non-trailing segment (e.g. "#/a//repos"),
// more than one trailing slash, an undecodable percent-escape, or extra
// trailing segments beyond what a route shape accepts -- falls back to that
// account's overview once a slug has been parsed, and to the global overview
// otherwise, including when the malformation is what prevents the slug
// itself from being parsed (e.g. "#/a//acme").

import type { FindingStatus, ReviewStatus, Severity } from './types';
import { SEVERITIES } from './format';

export const REVIEW_TABS = ['summary', 'diff', 'conversation', 'timeline', 'raw', 'usage'] as const;
export type ReviewTab = (typeof REVIEW_TABS)[number];

function isReviewTab(v: string | undefined): v is ReviewTab {
  return v !== undefined && (REVIEW_TABS as readonly string[]).includes(v);
}

export const PULL_OUTCOMES: readonly ReviewStatus[] = ['running', 'prepared', 'completed', 'superseded', 'skipped', 'capped', 'failed', 'canceled'];

// PullFilter is the pull list's filters, carried in the hash's query so a
// filtered list can be linked and comes back with Back. A field is present
// only when it differs from the default: open pulls, any last review
// outcome, every repository and author, no search.
export interface PullFilter {
  state?: 'closed' | 'all';
  outcome?: ReviewStatus;
  repo?: string;
  author?: string;
  q?: string;
}

// pullFilter drops the fields of f that are a default, so each filter has
// one Route and one URL; undefined when nothing is left.
export function pullFilter(f: { state?: string; outcome?: string; repo?: string; author?: string; q?: string }): PullFilter | undefined {
  const out: PullFilter = {};
  if (f.state === 'closed' || f.state === 'all') out.state = f.state;
  const outcome = PULL_OUTCOMES.find((o) => o === f.outcome);
  if (outcome) out.outcome = outcome;
  if (f.repo) out.repo = f.repo;
  if (f.author) out.author = f.author;
  if (f.q) out.q = f.q;
  return Object.keys(out).length ? out : undefined;
}

// FindingFilter is the findings list's filters, in the hash's query like a
// PullFilter's; a field is present only when it narrows the list.
export interface FindingFilter {
  severity?: Severity;
  status?: FindingStatus;
  repo?: string;
  q?: string;
}

export const FINDING_STATUSES: readonly FindingStatus[] = ['open', 'addressed'];

// findingFilter drops the fields of f that match everything; undefined when
// nothing is left.
export function findingFilter(f: { severity?: string; status?: string; repo?: string; q?: string }): FindingFilter | undefined {
  const out: FindingFilter = {};
  const severity = SEVERITIES.find((s) => s === f.severity);
  if (severity) out.severity = severity;
  const status = FINDING_STATUSES.find((s) => s === f.status);
  if (status) out.status = status;
  if (f.repo) out.repo = f.repo;
  if (f.q) out.q = f.q;
  return Object.keys(out).length ? out : undefined;
}

export type Route =
  | { name: 'overview' }
  | { name: 'signin' }
  | { name: 'console' }
  | { name: 'account'; slug: string }
  | { name: 'repos'; slug: string }
  | { name: 'repo'; slug: string; owner: string; repo: string }
  | { name: 'pulls'; slug: string; filter?: PullFilter }
  | { name: 'pull'; slug: string; owner: string; repo: string; number: number }
  | { name: 'review'; slug: string; id: string; tab?: ReviewTab }
  | { name: 'findings'; slug: string; filter?: FindingFilter }
  | { name: 'rules'; slug: string }
  | { name: 'queue'; slug: string }
  | { name: 'usage'; slug: string }
  | { name: 'followups'; slug: string }
  | { name: 'admin'; slug: string; section?: string };

const PULL_NUMBER = /^\d+$/;

// segments splits the part of the hash after "#/" on "/" and decodes each
// piece. A single trailing slash (exactly one trailing empty segment) is
// tolerated and dropped, so it decodes identically to the same hash without
// it. Any other malformation -- an empty non-trailing segment, more than one
// trailing slash, or an undecodable percent-escape -- stops decoding right
// there: `ok` is false and `parts` holds only what decoded cleanly before
// the bad segment, so a caller can still recover a slug that was fully
// parsed before the malformation struck.
function segments(hash: string): { parts: string[]; ok: boolean } {
  const stripped = hash.replace(/^#\/?/, '');
  if (stripped === '') return { parts: [], ok: true };
  const raw = stripped.split('/');
  if (raw.length > 1 && raw[raw.length - 1] === '') raw.pop();
  const decoded: string[] = [];
  for (const part of raw) {
    if (part === '') return { parts: decoded, ok: false };
    try {
      decoded.push(decodeURIComponent(part));
    } catch {
      return { parts: decoded, ok: false };
    }
  }
  return { parts: decoded, ok: true };
}

// parseAccountRoute handles everything under #/a/<forge>/<name>/... , the
// account's slug being "<forge>/<name>". Anything malformed past the slug --
// including an extra trailing segment -- falls back to that account's
// overview rather than the global overview, so a bad deep link still lands
// the user in-account.
function parseAccountRoute(slug: string, rest: string[], query: URLSearchParams): Route {
  const [section, ...tail] = rest;
  switch (section) {
    case undefined:
      return { name: 'account', slug };
    case 'repos':
      if (tail.length === 0) return { name: 'repos', slug };
      if (tail.length === 2) return { name: 'repo', slug, owner: tail[0]!, repo: tail[1]! };
      break;
    case 'pulls':
      if (tail.length === 0) {
        const filter = pullFilter(Object.fromEntries(query));
        return filter ? { name: 'pulls', slug, filter } : { name: 'pulls', slug };
      }
      if (tail.length === 3 && PULL_NUMBER.test(tail[2]!)) {
        return { name: 'pull', slug, owner: tail[0]!, repo: tail[1]!, number: Number(tail[2]) };
      }
      break;
    case 'reviews':
      if (tail.length === 1) return { name: 'review', slug, id: tail[0]! };
      if (tail.length === 2) return { name: 'review', slug, id: tail[0]!, tab: isReviewTab(tail[1]) ? tail[1] : undefined };
      break;
    case 'findings':
      if (tail.length === 0) {
        const filter = findingFilter(Object.fromEntries(query));
        return filter ? { name: 'findings', slug, filter } : { name: 'findings', slug };
      }
      break;
    case 'rules':
      if (tail.length === 0) return { name: 'rules', slug };
      break;
    case 'queue':
      if (tail.length === 0) return { name: 'queue', slug };
      break;
    case 'usage':
      if (tail.length === 0) return { name: 'usage', slug };
      break;
    case 'followups':
      if (tail.length === 0) return { name: 'followups', slug };
      break;
    case 'admin':
      if (tail.length === 0) return { name: 'admin', slug };
      if (tail.length === 1) return { name: 'admin', slug, section: tail[0] };
      break;
  }
  return { name: 'account', slug };
}

export function parse(hash: string): Route {
  const q = hash.indexOf('?');
  const { parts, ok } = segments(q < 0 ? hash : hash.slice(0, q));
  const slug = parts[0] === 'a' && parts[1] !== undefined && parts[2] !== undefined ? `${parts[1]}/${parts[2]}` : undefined;
  if (!ok) {
    // The malformation struck before a slug could be parsed: nothing to
    // fall back into but the global overview. Once a slug WAS parsed, the
    // malformation is downstream of it (a bad section, an empty segment,
    // extra segments, ...), so fall back to that account's own overview
    // instead.
    return slug ? { name: 'account', slug } : { name: 'overview' };
  }
  if (parts.length === 0) return { name: 'overview' };
  if (parts.length === 1 && parts[0] === 'signin') return { name: 'signin' };
  if (parts.length === 1 && parts[0] === 'admin') return { name: 'console' };
  if (slug) return parseAccountRoute(slug, parts.slice(3), new URLSearchParams(q < 0 ? '' : hash.slice(q + 1)));
  return { name: 'overview' };
}

// slugPath is an account slug, "<forge>/<name>", as two path segments.
export function slugPath(slug: string): string {
  const i = slug.indexOf('/');
  return i < 0 ? encodeURIComponent(slug) : `${encodeURIComponent(slug.slice(0, i))}/${encodeURIComponent(slug.slice(i + 1))}`;
}

export function href(r: Route): string {
  const s = (v: string) => encodeURIComponent(v);
  switch (r.name) {
    case 'overview':
      return '#/';
    case 'signin':
      return '#/signin';
    case 'console':
      return '#/admin';
    case 'account':
      return `#/a/${slugPath(r.slug)}`;
    case 'repos':
      return `#/a/${slugPath(r.slug)}/repos`;
    case 'repo':
      return `#/a/${slugPath(r.slug)}/repos/${s(r.owner)}/${s(r.repo)}`;
    case 'pulls': {
      const query = new URLSearchParams(Object.entries(r.filter ?? {})).toString();
      return `#/a/${slugPath(r.slug)}/pulls${query ? `?${query}` : ''}`;
    }
    case 'pull':
      return `#/a/${slugPath(r.slug)}/pulls/${s(r.owner)}/${s(r.repo)}/${r.number}`;
    case 'review':
      return r.tab ? `#/a/${slugPath(r.slug)}/reviews/${s(r.id)}/${s(r.tab)}` : `#/a/${slugPath(r.slug)}/reviews/${s(r.id)}`;
    case 'findings': {
      const query = new URLSearchParams(Object.entries(r.filter ?? {})).toString();
      return `#/a/${slugPath(r.slug)}/findings${query ? `?${query}` : ''}`;
    }
    case 'rules':
      return `#/a/${slugPath(r.slug)}/rules`;
    case 'queue':
      return `#/a/${slugPath(r.slug)}/queue`;
    case 'usage':
      return `#/a/${slugPath(r.slug)}/usage`;
    case 'followups':
      return `#/a/${slugPath(r.slug)}/followups`;
    case 'admin':
      return r.section ? `#/a/${slugPath(r.slug)}/admin/${s(r.section)}` : `#/a/${slugPath(r.slug)}/admin`;
  }
}
