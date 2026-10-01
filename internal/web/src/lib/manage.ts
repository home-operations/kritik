// Turning management API errors into what the dashboard tells the user.
// The server's message is already human-readable; these add what to do
// next where the code alone says more than the message does.
import { ApiError } from './api.svelte';
import type { ErrorCode, ManagementErrorCode } from './types';

const hints: Partial<Record<ManagementErrorCode | ErrorCode, string>> = {
  forge_error: 'GitHub refused or failed the request.',
  installation_served: 'The App serves this account.',
  already_queued: 'That is already queued or running; it will show up here when it finishes.',
  unauthenticated: 'Your session has ended; sign in again.',
  csrf: 'The request was refused as not coming from this page; reload and try again.',
  not_cancelable: 'The review is no longer running.',
  no_head: 'The pull request has no known head to review.',
  forbidden: 'You are not allowed to do this.',
};

// describe is one line for a failed management call.
export function describe(err: unknown): string {
  if (!(err instanceof ApiError)) return err instanceof Error ? err.message : String(err);
  const hint = hints[err.code as ManagementErrorCode | ErrorCode];
  if (!hint) return err.message || `request failed (${err.status})`;
  return err.message && err.message !== hint ? `${hint} ${err.message}` : hint;
}

export function isCode(err: unknown, code: ManagementErrorCode): boolean {
  return err instanceof ApiError && err.code === code;
}
