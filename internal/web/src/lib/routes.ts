// Pure, rune-free route parsing/serialization for the hash router. Kept out
// of router.svelte.ts (which needs the .svelte.ts extension because of its
// `$state` rune) so these functions can be imported by tooling that doesn't
// go through Svelte's compiler -- e.g. a plain Playwright test.
//
//   #/                                        overview (tenant picker / landing)
//   #/signin                                  sign-in page
//   #/operator                                operator console (cross-tenant)
//   #/t/<slug>                                tenant overview
//   #/t/<slug>/repos                          tenant's repo list
//   #/t/<slug>/repos/<owner>/<repo>           one repo
//   #/t/<slug>/pulls                          tenant's pull list
//   #/t/<slug>/pulls/<owner>/<repo>/<n>       one pull request
//   #/t/<slug>/reviews/<id>[/<tab>]           one review, optional tab
//   #/t/<slug>/queue                          run queue
//   #/t/<slug>/usage                          usage/cost dashboard
//   #/t/<slug>/followups                      follow-up tracker
//   #/t/<slug>/admin[/<section>]              tenant admin, optional section
//
// Segments round-trip through encodeURIComponent/decodeURIComponent, so a
// slug/owner/repo/id/section containing a literal "/" or other reserved
// character survives href() -> parse(). A malformed hash -- an empty segment
// (e.g. "#/t//repos", or a trailing slash producing a trailing empty
// segment), an undecodable percent-escape, or extra trailing segments beyond
// what a route shape accepts -- falls back the same way an unrecognized one
// does: to that tenant's overview once a slug has been parsed, otherwise to
// the global overview.

export const REVIEW_TABS = ['summary', 'diff', 'conversation', 'timeline', 'raw', 'usage'] as const;
export type ReviewTab = (typeof REVIEW_TABS)[number];

function isReviewTab(v: string | undefined): v is ReviewTab {
  return v !== undefined && (REVIEW_TABS as readonly string[]).includes(v);
}

export type Route =
  | { name: 'overview' }
  | { name: 'signin' }
  | { name: 'operator' }
  | { name: 'tenant'; slug: string }
  | { name: 'repos'; slug: string }
  | { name: 'repo'; slug: string; owner: string; repo: string }
  | { name: 'pulls'; slug: string }
  | { name: 'pull'; slug: string; owner: string; repo: string; number: number }
  | { name: 'review'; slug: string; id: string; tab?: ReviewTab }
  | { name: 'queue'; slug: string }
  | { name: 'usage'; slug: string }
  | { name: 'followups'; slug: string }
  | { name: 'admin'; slug: string; section?: string };

const PULL_NUMBER = /^\d+$/;

// segments splits the part of the hash after "#/" on "/" and decodes each
// piece, returning undefined if the hash is malformed: an empty segment
// (adjacent or trailing slashes) or an undecodable percent-escape.
function segments(hash: string): string[] | undefined {
  const stripped = hash.replace(/^#\/?/, '');
  if (stripped === '') return [];
  const decoded: string[] = [];
  for (const part of stripped.split('/')) {
    if (part === '') return undefined;
    try {
      decoded.push(decodeURIComponent(part));
    } catch {
      return undefined;
    }
  }
  return decoded;
}

// parseTenantRoute handles everything under #/t/<slug>/... . Anything
// malformed past the slug -- including an extra trailing segment -- falls
// back to that tenant's overview rather than the global overview, so a bad
// deep link still lands the user in-tenant.
function parseTenantRoute(slug: string, rest: string[]): Route {
  const [section, ...tail] = rest;
  switch (section) {
    case undefined:
      return { name: 'tenant', slug };
    case 'repos':
      if (tail.length === 0) return { name: 'repos', slug };
      if (tail.length === 2) return { name: 'repo', slug, owner: tail[0]!, repo: tail[1]! };
      break;
    case 'pulls':
      if (tail.length === 0) return { name: 'pulls', slug };
      if (tail.length === 3 && PULL_NUMBER.test(tail[2]!)) {
        return { name: 'pull', slug, owner: tail[0]!, repo: tail[1]!, number: Number(tail[2]) };
      }
      break;
    case 'reviews':
      if (tail.length === 1) return { name: 'review', slug, id: tail[0]! };
      if (tail.length === 2) return { name: 'review', slug, id: tail[0]!, tab: isReviewTab(tail[1]) ? tail[1] : undefined };
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
  return { name: 'tenant', slug };
}

export function parse(hash: string): Route {
  const parts = segments(hash);
  if (parts === undefined || parts.length === 0) return { name: 'overview' };
  if (parts.length === 1 && parts[0] === 'signin') return { name: 'signin' };
  if (parts.length === 1 && parts[0] === 'operator') return { name: 'operator' };
  if (parts[0] === 't' && parts[1] !== undefined) return parseTenantRoute(parts[1], parts.slice(2));
  return { name: 'overview' };
}

export function href(r: Route): string {
  const s = (v: string) => encodeURIComponent(v);
  switch (r.name) {
    case 'overview':
      return '#/';
    case 'signin':
      return '#/signin';
    case 'operator':
      return '#/operator';
    case 'tenant':
      return `#/t/${s(r.slug)}`;
    case 'repos':
      return `#/t/${s(r.slug)}/repos`;
    case 'repo':
      return `#/t/${s(r.slug)}/repos/${s(r.owner)}/${s(r.repo)}`;
    case 'pulls':
      return `#/t/${s(r.slug)}/pulls`;
    case 'pull':
      return `#/t/${s(r.slug)}/pulls/${s(r.owner)}/${s(r.repo)}/${r.number}`;
    case 'review':
      return r.tab ? `#/t/${s(r.slug)}/reviews/${s(r.id)}/${s(r.tab)}` : `#/t/${s(r.slug)}/reviews/${s(r.id)}`;
    case 'queue':
      return `#/t/${s(r.slug)}/queue`;
    case 'usage':
      return `#/t/${s(r.slug)}/usage`;
    case 'followups':
      return `#/t/${s(r.slug)}/followups`;
    case 'admin':
      return r.section ? `#/t/${s(r.slug)}/admin/${s(r.section)}` : `#/t/${s(r.slug)}/admin`;
  }
}
