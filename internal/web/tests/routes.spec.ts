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
  { name: 'tenant', slug: 'acme' },
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
  { name: 'tenant', slug: 'a/b c#d' },
  { name: 'repo', slug: 'acme', owner: 'weird/owner', repo: 're po' },
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
  ['#/t', { name: 'overview' }],
  ['#/t/', { name: 'overview' }],
  ['#/t//repos', { name: 'overview' }],
  ['#/t/acme/', { name: 'overview' }],
  ['#/t/acme/repos/only-owner', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/repos/o/r/extra', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/pulls/o/r/abc', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/pulls/o/r/-5', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/pulls/o/r/3.5', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/pulls/o/r/1e2', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/queue/extra', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/admin/section/extra', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/reviews/r1/bogus', { name: 'review', slug: 'acme', id: 'r1' }],
  ['#/t/acme/reviews/r1/diff/extra', { name: 'tenant', slug: 'acme' }],
  ['#/t/acme/bogus-section', { name: 'tenant', slug: 'acme' }],
];

test.describe('routes: parse() on unknown/malformed hashes', () => {
  for (const [hash, expected] of MALFORMED) {
    test(`parse(${JSON.stringify(hash)})`, () => {
      expect(parse(hash)).toEqual(expected);
    });
  }
});
