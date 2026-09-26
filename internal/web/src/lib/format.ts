// Pure display formatting shared by every page. No runes, so tests and
// tooling can import it directly.
import type { JobState, ReviewStatus, Severity, IndexRunStatus, FollowupStatus } from './types';

const compact = new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 });
const whole = new Intl.NumberFormat('en');

// tokens renders a token count compactly ("12.3K"), with the exact figure
// available through wholeNumber for a title attribute.
export function tokens(n: number): string {
  return compact.format(n);
}

export function wholeNumber(n: number): string {
  return whole.format(n);
}

// usd renders a dollar cost; sub-cent amounts keep enough precision to read
// as non-zero since a single model call commonly costs a fraction of a cent.
export function usd(n: number): string {
  if (n === 0) return '$0';
  if (Math.abs(n) < 0.01) return `$${n.toFixed(4)}`;
  return `$${n.toFixed(2)}`;
}

export function duration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return '';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return rem ? `${m}m ${rem}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

// between is the duration between two RFC 3339 timestamps, null when either
// is missing or unparsable.
export function between(from: string | null | undefined, to: string | null | undefined): number | null {
  if (!from || !to) return null;
  const a = Date.parse(from);
  const b = Date.parse(to);
  if (Number.isNaN(a) || Number.isNaN(b)) return null;
  return Math.max(0, b - a);
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}

export function shortSha(sha: string): string {
  return sha.slice(0, 7);
}

export type Tone = 'ok' | 'danger' | 'warn' | 'accent' | 'merged' | 'muted';

export const reviewTone: Record<ReviewStatus, Tone> = {
  running: 'accent',
  prepared: 'accent',
  completed: 'ok',
  superseded: 'muted',
  skipped: 'muted',
  capped: 'warn',
  failed: 'danger',
  canceled: 'muted',
};

export const jobTone: Record<JobState, Tone> = {
  available: 'accent',
  scheduled: 'muted',
  running: 'accent',
  retryable: 'warn',
  pending: 'muted',
  completed: 'ok',
  cancelled: 'muted',
  discarded: 'danger',
};

export const indexTone: Record<IndexRunStatus, Tone> = {
  running: 'accent',
  completed: 'ok',
  failed: 'danger',
  superseded: 'muted',
};

export const followupTone: Record<FollowupStatus, Tone> = {
  answered: 'ok',
  limited: 'warn',
  ignored: 'muted',
  failed: 'danger',
};

export const SEVERITIES: readonly Severity[] = ['blocking', 'important', 'nit'];

// isActive reports whether a review is still moving, i.e. its page should
// follow live events.
export function isActive(s: ReviewStatus): boolean {
  return s === 'running' || s === 'prepared';
}

// daysAgo returns the YYYY-MM-DD date `days` before `now`, in UTC.
export function daysAgo(days: number, now: number): string {
  return new Date(now - days * 86_400_000).toISOString().slice(0, 10);
}

// splitRepo splits "owner/name" at its first slash.
export function splitRepo(fullName: string): { owner: string; repo: string } {
  const i = fullName.indexOf('/');
  return i < 0 ? { owner: fullName, repo: '' } : { owner: fullName.slice(0, i), repo: fullName.slice(i + 1) };
}

// pretty renders a JSON value indented; strings that are themselves JSON are
// left as-is so a raw passthrough doesn't get double-quoted.
export function pretty(v: unknown): string {
  if (typeof v === 'string') return v;
  try {
    return JSON.stringify(v, null, 2) ?? String(v);
  } catch {
    return String(v);
  }
}
