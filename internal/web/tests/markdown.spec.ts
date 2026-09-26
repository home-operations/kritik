// markdown.ts is pure (no runes), so it is tested directly with no browser.
// These pin the safety contract: only http(s) links survive as links, and
// nothing in the source can become markup since the renderer only ever
// interpolates text and a validated href.
import { test, expect } from '@playwright/test';
import { parseInline, parseMarkdown, safeHref } from '../src/lib/markdown';

test.describe('markdown: links', () => {
  for (const href of ['javascript:alert(1)', 'JaVaScRiPt:alert(1)', 'data:text/html,<b>x</b>', 'vbscript:x', '/relative', 'relative/path', '#frag', '//evil.example/x']) {
    test(`${href} is not a link`, () => {
      expect(safeHref(href)).toBeUndefined();
      const out = parseInline(`see [here](${href}) now`);
      expect(out.some((t) => t.kind === 'link')).toBe(false);
      expect(out.map((t) => t.text).join('')).toContain('[here]');
    });
  }

  test('http and https links survive, normalised', () => {
    expect(parseInline('[a](https://example.com/x) [b](http://example.com)')).toEqual([
      { kind: 'link', text: 'a', href: 'https://example.com/x' },
      { kind: 'text', text: ' ' },
      { kind: 'link', text: 'b', href: 'http://example.com/' },
    ]);
  });

  test('quotes and angle brackets in an href are percent-encoded, not attribute breakers', () => {
    const [link] = parseInline('[x](https://example.com/"onmouseover="alert(1)<b>)');
    expect(link?.kind).toBe('link');
    if (link?.kind !== 'link') return;
    expect(link.href).not.toContain('"');
    expect(link.href).not.toContain('<');
  });
});

test.describe('markdown: blocks and inline', () => {
  test('html in text stays text', () => {
    expect(parseInline('<img src=x onerror=alert(1)>')).toEqual([{ kind: 'text', text: '<img src=x onerror=alert(1)>' }]);
  });

  test('code spans, bold and fenced blocks', () => {
    expect(parseInline('use `x != nil` **now**')).toEqual([
      { kind: 'text', text: 'use ' },
      { kind: 'code', text: 'x != nil' },
      { kind: 'text', text: ' ' },
      { kind: 'bold', text: 'now' },
    ]);
    expect(parseMarkdown('para one\n\n```go\nfunc f() {}\n```\nafter')).toEqual([
      { kind: 'para', inlines: [{ kind: 'text', text: 'para one' }] },
      { kind: 'code', lang: 'go', text: 'func f() {}' },
      { kind: 'para', inlines: [{ kind: 'text', text: 'after' }] },
    ]);
  });

  test('an unterminated fence swallows the rest as code', () => {
    expect(parseMarkdown('```\nx\ny')).toEqual([{ kind: 'code', lang: '', text: 'x\ny' }]);
  });
});
