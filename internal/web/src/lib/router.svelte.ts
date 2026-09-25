// A tiny hash router so every dashboard view is deep-linkable and survives
// refresh / browser back. Every tenant-scoped route carries the tenant's
// slug (from Me.tenants[].slug) so switching tenants is just a URL rewrite.
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
// `parse`/`href` are pure and unit-tested directly (tests/router.spec.ts);
// everything else is thin state wiring around them.

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

// parseTenantRoute handles everything under #/t/<slug>/... . Anything
// malformed past the slug falls back to that tenant's overview rather than
// the global overview, so a bad deep link still lands the user in-tenant.
function parseTenantRoute(slug: string, rest: string[]): Route {
  const [section, ...tail] = rest;
  switch (section) {
    case undefined:
      return { name: 'tenant', slug };
    case 'repos':
      if (tail.length === 0) return { name: 'repos', slug };
      if (tail.length === 2 && tail[0] && tail[1]) {
        return { name: 'repo', slug, owner: tail[0], repo: tail[1] };
      }
      break;
    case 'pulls':
      if (tail.length === 0) return { name: 'pulls', slug };
      if (tail.length === 3 && tail[0] && tail[1]) {
        const number = Number(tail[2]);
        if (Number.isInteger(number) && number > 0) {
          return { name: 'pull', slug, owner: tail[0], repo: tail[1], number };
        }
      }
      break;
    case 'reviews':
      if (tail[0]) return { name: 'review', slug, id: tail[0], tab: isReviewTab(tail[1]) ? tail[1] : undefined };
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
      return { name: 'admin', slug, section: tail[0] };
    default:
      break;
  }
  return { name: 'tenant', slug };
}

export function parse(hash: string): Route {
  const parts = hash.replace(/^#\/?/, '').split('/').filter(Boolean);
  if (parts.length === 0) return { name: 'overview' };
  if (parts[0] === 'signin' && parts.length === 1) return { name: 'signin' };
  if (parts[0] === 'operator' && parts.length === 1) return { name: 'operator' };
  if (parts[0] === 't' && parts[1]) return parseTenantRoute(parts[1], parts.slice(2));
  return { name: 'overview' };
}

export function href(r: Route): string {
  switch (r.name) {
    case 'overview':
      return '#/';
    case 'signin':
      return '#/signin';
    case 'operator':
      return '#/operator';
    case 'tenant':
      return `#/t/${r.slug}`;
    case 'repos':
      return `#/t/${r.slug}/repos`;
    case 'repo':
      return `#/t/${r.slug}/repos/${r.owner}/${r.repo}`;
    case 'pulls':
      return `#/t/${r.slug}/pulls`;
    case 'pull':
      return `#/t/${r.slug}/pulls/${r.owner}/${r.repo}/${r.number}`;
    case 'review':
      return r.tab ? `#/t/${r.slug}/reviews/${r.id}/${r.tab}` : `#/t/${r.slug}/reviews/${r.id}`;
    case 'queue':
      return `#/t/${r.slug}/queue`;
    case 'usage':
      return `#/t/${r.slug}/usage`;
    case 'followups':
      return `#/t/${r.slug}/followups`;
    case 'admin':
      return r.section ? `#/t/${r.slug}/admin/${r.section}` : `#/t/${r.slug}/admin`;
  }
}

export const router = $state<{ route: Route }>({ route: parse(location.hash) });

export function navigate(to: Route): void {
  const next = href(to);
  if (location.hash === next) {
    router.route = to; // same hash (e.g. re-selecting the current tab) — sync anyway
  } else {
    location.hash = next; // triggers hashchange → updates router.route
  }
}

// replace updates the route without a history entry.
export function replace(to: Route): void {
  router.route = to;
  try {
    history.replaceState(null, '', href(to));
  } catch {
    // Safari rate-limits replaceState (SecurityError past ~100 calls/30s).
    // The in-memory route is already updated; the URL syncs on the next call.
  }
}

export function initRouter(): void {
  window.addEventListener('hashchange', () => {
    router.route = parse(location.hash);
  });
}
