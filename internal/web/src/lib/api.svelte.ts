// A thin fetch wrapper: every call is same-origin with credentials, tags
// itself so a reverse proxy or WAF can tell it apart from a browser
// navigation, and turns a non-2xx response into a typed ApiError instead of
// forcing every call site to check res.ok. A 401 means the session died
// (expired cookie, revoked token) — redirect to sign-in without a history
// entry so "back" doesn't bounce the user right back into the 401.
import { basePath } from './base';
import { parse, replace } from './router.svelte';
import { session } from './session.svelte';

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: unknown;

  constructor(status: number, code: string, message: string, details?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

interface ErrorBody {
  code?: string;
  message?: string;
  details?: unknown;
}

const commonHeaders: HeadersInit = { 'X-Kritik': '1' };

// Where SignIn.svelte should send the user back to after signing in: the
// hash they were on when a 401 bounced them, captured before replace()
// overwrites it with #/signin. Never a sign-in route itself: a 401 while
// already on sign-in doesn't redirect, so returnTo keeps its '' default and
// SignIn.svelte falls back to '#/'.
export const signinState = $state<{ returnTo: string }>({ returnTo: '' });

async function toApiError(res: Response): Promise<ApiError> {
  let body: ErrorBody = {};
  try {
    body = (await res.json()) as ErrorBody;
  } catch {
    // No JSON body (e.g. a proxy error page) — fall back to the status text.
  }
  return new ApiError(res.status, body.code ?? 'unknown', body.message ?? res.statusText, body.details);
}

// toSignIn sends the tab to sign-in, remembering where it was, unless it
// is already there. The session is forgotten first: the shell sends a
// signed-in user off the sign-in page, straight back into the 401.
export function toSignIn(): void {
  session.me = undefined;
  if (parse(location.hash).name === 'signin') return;
  signinState.returnTo = location.hash || '#/';
  replace({ name: 'signin' });
}

async function handle<T>(res: Response): Promise<T> {
  // The dashboard has no public content: a 401 means the session is dead
  // (expired cookie, revoked token, or no session at all) and every route --
  // including the bare root, which has no hash -- redirects to sign-in. The
  // only guard is against a redirect loop when already on sign-in, decided
  // by the router's parse so every spelling it accepts (e.g. "#/signin/")
  // counts.
  if (res.status === 401) toSignIn();
  if (!res.ok) throw await toApiError(res);
  if (res.status === 204) return undefined as T;
  try {
    return (await res.json()) as T;
  } catch {
    throw new ApiError(res.status, 'invalid_response', 'response was not valid JSON');
  }
}

export async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${basePath}${path}`, {
    method: 'GET',
    headers: commonHeaders,
    credentials: 'same-origin',
  });
  return handle<T>(res);
}

export type WriteMethod = 'POST' | 'PUT' | 'PATCH' | 'DELETE';

export async function sendJSON<T>(method: WriteMethod, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${basePath}${path}`, {
    method,
    headers: { ...commonHeaders, 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return handle<T>(res);
}
