// A tiny hash router so every dashboard view is deep-linkable and survives
// refresh / browser back. Every tenant-scoped route carries the tenant's
// slug (from Me.tenants[].slug) so switching tenants is just a URL rewrite.
//
// `Route`/`REVIEW_TABS`/`parse`/`href` are pure and live in routes.ts (no
// runes, so they're importable outside Svelte's compiler -- see
// tests/routes.spec.ts for the round-trip and malformed-hash coverage).
// This file is just the `$state` wiring around them, which is why it needs
// the .svelte.ts extension.

export { REVIEW_TABS, parse, href } from './routes';
export type { Route, ReviewTab } from './routes';

import { href, parse, type Route } from './routes';

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
