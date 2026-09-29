// The pull request list's search box: free text, plus filter tokens such as
// "repo:owner/name", which it reads into a PullFilter and writes back from
// one. Rune-free so tests can import it.
import { PULL_OUTCOMES, type PullFilter } from './routes';
import type { ReviewStatus } from './types';

export const TOKENS = [
  { key: 'repo', hint: 'a repository' },
  { key: 'author', hint: "an author's login" },
  { key: 'status', hint: "the last review's status" },
] as const;

export type TokenKey = (typeof TOKENS)[number]['key'];

// SearchFields are what the box sets of a PullFilter; the state is set
// beside it.
export type SearchFields = Pick<PullFilter, 'repo' | 'author' | 'outcome' | 'q'>;

export interface Parsed {
  fields: SearchFields;
  // unknown are the tokens whose value names no repository or status, so
  // the list is not filtered by them.
  unknown: string[];
}

const TOKEN = /^(repo|author|status):(\S+)$/i;

// parseSearch reads text; repos are the account's repositories, which a
// repo: token must name. The last of a repeated token wins.
export function parseSearch(text: string, repos: readonly string[]): Parsed {
  const fields: SearchFields = {};
  const unknown: string[] = [];
  const free: string[] = [];
  for (const word of text.split(/\s+/).filter(Boolean)) {
    const m = TOKEN.exec(word);
    if (!m) {
      free.push(word);
      continue;
    }
    const key = m[1]!.toLowerCase() as TokenKey;
    const value = m[2]!;
    if (key === 'author') {
      fields.author = value;
    } else if (key === 'repo') {
      const repo = repos.find((r) => r.toLowerCase() === value.toLowerCase());
      if (repo) fields.repo = repo;
      else unknown.push(word);
    } else {
      const outcome = PULL_OUTCOMES.find((o) => o === value.toLowerCase());
      if (outcome) fields.outcome = outcome;
      else unknown.push(word);
    }
  }
  if (free.length) fields.q = free.join(' ');
  return { fields, unknown };
}

// formatSearch writes the box's text for a filter, tokens first.
export function formatSearch(f: SearchFields | undefined): string {
  if (!f) return '';
  const words: string[] = [];
  if (f.repo) words.push(`repo:${f.repo}`);
  if (f.author) words.push(`author:${f.author}`);
  if (f.outcome) words.push(`status:${f.outcome}`);
  if (f.q) words.push(f.q);
  return words.join(' ');
}

export interface Suggestion {
  label: string;
  hint?: string;
  // text replaces the word being typed.
  text: string;
}

// suggest completes the last word of text: a token's key while it has no
// colon, then the token's values.
export function suggest(text: string, values: { repos: readonly string[]; authors: readonly string[] }): Suggestion[] {
  const word = /\S*$/.exec(text)![0];
  const colon = word.indexOf(':');
  if (colon < 0) {
    const w = word.toLowerCase();
    return TOKENS.filter((t) => t.key.startsWith(w)).map((t) => ({ label: `${t.key}:`, hint: t.hint, text: `${t.key}:` }));
  }
  const key = word.slice(0, colon).toLowerCase();
  const partial = word.slice(colon + 1).toLowerCase();
  const pool: readonly string[] =
    key === 'repo' ? values.repos : key === 'author' ? values.authors : key === 'status' ? (PULL_OUTCOMES as readonly ReviewStatus[]) : [];
  return pool
    .filter((v) => v.toLowerCase().includes(partial) && v.toLowerCase() !== partial)
    .slice(0, 8)
    .map((v) => ({ label: `${key}:${v}`, text: `${key}:${v} ` }));
}

// complete replaces the last word of text with a suggestion.
export function complete(text: string, s: Suggestion): string {
  return text.replace(/\S*$/, s.text);
}
