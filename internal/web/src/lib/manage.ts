// Turning management API errors into what the dashboard tells the user.
// The server's message is already human-readable; these add what to do
// next where the code alone says more than the message does.
import { ApiError } from './api.svelte';
import type { ConfigSource, ErrorCode, FieldPolicy, ManagementErrorCode, PathDetails } from './types';

const hints: Partial<Record<ManagementErrorCode | ErrorCode, string>> = {
  revision_conflict: 'Someone else saved the configuration since you loaded it.',
  reenter_secret: "What this secret acts for changed, so it must be entered again.",
  reindex_required: "A new embedding model or dimension rebuilds every repository's index.",
  forge_error: 'GitHub refused or failed the request.',
  installation_served: 'The connection serves this account.',
  config_blocked: 'The running configuration is invalid elsewhere; an admin must fix it before this can be saved.',
  management_disabled: 'Dashboard management is disabled: no sealing key configured.',
  actions_disabled: 'This server process cannot queue dashboard actions.',
  already_queued: 'That is already queued or running; it will show up here when it finishes.',
  unauthenticated: 'Your session has ended; sign in again.',
  csrf: 'The request was refused as not coming from this page; reload and try again.',
  not_cancelable: 'The review is no longer running.',
  no_head: 'The pull request has no known head to review.',
  forbidden: 'You are not allowed to do this.',
};

// describe is one line for a failed management call.
const inheritedFrom: Record<ConfigSource, string> = {
  default: "kritik's default",
  env: 'the environment',
  file: 'the config file',
  dashboard: 'the instance settings',
  defaults: 'the defaults',
  account: 'this account',
  repository: '.kritik.yaml',
};

// inheritsHint is what a field left empty takes, and from where.
export function inheritsHint(value: string, source: ConfigSource | undefined): string {
  return `inherits ${value} from ${inheritedFrom[source ?? 'default']}`;
}

// fieldEditable says whether the policy lets the caller change key: every
// setting it covers, itself or nested under it, is editable.
export function fieldEditable(policy: FieldPolicy[], key: string): boolean {
  const rows = policy.filter((p) => p.key === key || p.key.startsWith(`${key}.`));
  return rows.length > 0 && rows.every((p) => p.editable);
}

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
