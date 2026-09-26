// Turning management API errors into what the dashboard tells the user.
// The server's message is already human-readable; these add what to do
// next where the code alone says more than the message does.
import { ApiError } from './api.svelte';
import type { ErrorCode, ManagementErrorCode, PathDetails } from './types';

const hints: Partial<Record<ManagementErrorCode | ErrorCode, string>> = {
  revision_conflict: 'Someone else saved this tenant since you loaded it.',
  operator_only: 'Only an instance operator can change this field.',
  reenter_secret: "The installation's forge, host or account changed, so this secret must be entered again.",
  slug_taken: 'That name is already in use.',
  config_blocked: 'The running configuration is invalid elsewhere; an operator must fix it before this can be saved.',
  file_managed: 'This tenant is declared in the configuration file and cannot be changed here.',
  management_disabled: 'Dashboard management is disabled: no sealing key configured.',
  actions_disabled: 'This server process cannot queue dashboard actions.',
  already_queued: 'That is already queued or running; it will show up here when it finishes.',
  unauthenticated: 'Your session has ended; sign in again.',
  csrf: 'The request was refused as not coming from this page; reload and try again.',
  already_member: 'That account is already a member; change its role instead.',
  invite_exists: 'A pending invite for this email already exists.',
  last_admin: 'The tenant would be left with no admin.',
  not_invite_member: "This member's access comes only from the forge.",
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

// errorPath is the spec path an error points at, "" when none.
export function errorPath(err: unknown): string {
  if (!(err instanceof ApiError)) return '';
  const d = err.details as Partial<PathDetails> | undefined;
  return typeof d?.path === 'string' ? d.path : '';
}

export function isCode(err: unknown, code: ManagementErrorCode): boolean {
  return err instanceof ApiError && err.code === code;
}
