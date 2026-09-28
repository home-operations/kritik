// Pure round-trip and malformed-input coverage for parse()/href() in
// routes.ts. These are plain functions with no Svelte runes, so -- unlike
// router.spec.ts, which exercises the $state-based router.svelte.ts through
// a real page -- this file calls them directly and needs no browser.
import { test, expect } from '@playwright/test';
import { href, parse, type Route } from '../src/lib/routes';

const ROUTES: Route[] = [
  { name: 'overview' },
  { name: 'signin' },
  { name: 'operator' },
  { name: 'account', slug: 'acme' },
  { name: 'repos', slug: 'acme' },
  { name: 'repo', slug: 'acme', owner: 'kritik', repo: 'kritik' },
  { name: 'pulls', slug: 'acme' },
  { name: 'pull', slug: 'acme', owner: 'kritik', repo: 'kritik', number: 42 },
  { name: 'review', slug: 'acme', id: 'r1' },
  { name: 'review', slug: 'acme', id: 'r1', tab: 'diff' },
  { name: 'queue', slug: 'acme' },
  { name: 'usage', slug: 'acme' },
  { name: 'followups', slug: 'acme' },
  { name: 'admin', slug: 'acme' },
  { name: 'admin', slug: 'acme', section: 'tokens' },
  // segments containing characters that must round-trip through
  // encodeURIComponent/decodeURIComponent (slashes, spaces, '#').
  { name: 'account', slug: 'a/b c#d' },
  { name: 'repo', slug: 'acme', owner: 'weird/owner', repo: 're po' },
  // a connection, naming which of several holding owner/repo is meant.
  { name: 'repo', slug: 'acme', owner: 'kritik', repo: 'kritik', connection: 'acme-other' },
  { name: 'pull', slug: 'acme', owner: 'kritik', repo: 'kritik', number: 42, connection: 'a b&c' },
];

test.describe('routes: parse(href(r)) === r', () => {
  for (const route of ROUTES) {
    test(JSON.stringify(route), () => {
      expect(parse(href(route))).toEqual(route);
    });
  }
});

const MALFORMED: [string, Route][] = [
  ['', { name: 'overview' }],
  ['#/', { name: 'overview' }],
  ['#/nonsense', { name: 'overview' }],
  ['#/signin/extra', { name: 'overview' }],
  ['#/operator/extra', { name: 'overview' }],
  ['#/a', { name: 'overview' }],
  ['#/a/', { name: 'overview' }],
  ['#/a//repos', { name: 'overview' }],
  ['#/a//acme', { name: 'overview' }],
  // A single trailing slash is tolerated and parses like its absence, even
  // on a bare account slug -- this is no longer "malformed" so much as an
  // accepted alternate spelling.
  ['#/a/acme/', { name: 'account', slug: 'acme' }],
  ['#/a/acme/repos/only-owner', { name: 'account', slug: 'acme' }],
  ['#/a/acme/repos/o/r/extra', { name: 'account', slug: 'acme' }],
  // Same case as above, but with a tolerated trailing slash: still falls
  // back to the account overview, not the global one.
  ['#/a/acme/repos/o/r/extra/', { name: 'account', slug: 'acme' }],
  // A double slash after the slug is downstream of it, so it falls back to
  // that account's overview rather than the global one.
  ['#/a/acme//repos', { name: 'account', slug: 'acme' }],
  ['#/a/acme/pulls/o/r/abc', { name: 'account', slug: 'acme' }],
  ['#/a/acme/pulls/o/r/-5', { name: 'account', slug: 'acme' }],
  ['#/a/acme/pulls/o/r/3.5', { name: 'account', slug: 'acme' }],
  ['#/a/acme/pulls/o/r/1e2', { name: 'account', slug: 'acme' }],
  ['#/a/acme/queue/extra', { name: 'account', slug: 'acme' }],
  ['#/a/acme/admin/section/extra', { name: 'account', slug: 'acme' }],
  ['#/a/acme/reviews/r1/bogus', { name: 'review', slug: 'acme', id: 'r1' }],
  ['#/a/acme/reviews/r1/diff/extra', { name: 'account', slug: 'acme' }],
  ['#/a/acme/bogus-section', { name: 'account', slug: 'acme' }],
  // An empty or absent connection names none; only repo and pull routes
  // take one.
  ['#/a/acme/repos/o/r?connection=', { name: 'repo', slug: 'acme', owner: 'o', repo: 'r' }],
  ['#/a/acme/pulls/o/r/7?other=1', { name: 'pull', slug: 'acme', owner: 'o', repo: 'r', number: 7 }],
  ['#/a/acme/queue?connection=x', { name: 'queue', slug: 'acme' }],
];

test.describe('routes: parse() on unknown/malformed hashes', () => {
  for (const [hash, expected] of MALFORMED) {
    test(`parse(${JSON.stringify(hash)})`, () => {
      expect(parse(hash)).toEqual(expected);
    });
  }
});
