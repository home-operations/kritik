// Server-sent events keep the dashboard live: run status changes, review
// completions, follow-up updates, and the like. One shared EventSource per
// page load, reconnected with the same exponential-backoff-with-jitter
// konflate's websocket uses, so a server restart doesn't reconnect every open
// tab in lockstep a couple seconds later.
import { basePath } from './base';

type Listener = (data: unknown) => void;

const listeners = new Map<string, Set<Listener>>();
let source: EventSource | null = null;
let attempt = 0;

function parseData(e: MessageEvent<string>): unknown {
  try {
    return JSON.parse(e.data) as unknown;
  } catch {
    return e.data;
  }
}

function dispatch(kind: string, e: MessageEvent<string>): void {
  const data = parseData(e);
  for (const fn of listeners.get(kind) ?? []) fn(data);
}

// attachKind wires one server-sent event "kind" (the `event:` field) to
// dispatch. Called once per kind per connection: on a fresh subscribe if
// already connected, and for every known kind when a new EventSource opens
// after a reconnect.
function attachKind(kind: string): void {
  source?.addEventListener(kind, (e) => dispatch(kind, e as MessageEvent<string>));
}

function scheduleReconnect(): void {
  // delay ∈ [base/2, base), base = 1s·2^attempt capped at 30s. Every client
  // otherwise reconnects in lockstep shortly after a server restart.
  const base = Math.min(30_000, 1_000 * 2 ** attempt);
  attempt++;
  setTimeout(connect, base / 2 + Math.random() * (base / 2));
}

function connect(): void {
  source = new EventSource(`${basePath}/api/events`);
  source.addEventListener('open', () => {
    attempt = 0;
  });
  // EventSource retries on its own after 'error', but only at a fixed
  // interval; close it and drive the reconnect ourselves so the backoff
  // above actually applies.
  source.addEventListener('error', () => {
    source?.close();
    scheduleReconnect();
  });
  for (const kind of listeners.keys()) attachKind(kind);
}

export function initEvents(): void {
  if (!source) connect();
}

// closeEvents tears down the shared connection on sign-out, so a stale
// session cookie doesn't keep streaming another account's events into a
// signed-out tab. initEvents() reconnects cleanly on the next sign-in.
export function closeEvents(): void {
  source?.close();
  source = null;
  attempt = 0;
}

// subscribe registers fn for events of the given kind, returning an
// unsubscribe function. Safe to call before initEvents(): the listener is
// attached to the EventSource once one exists (immediately, or on the next
// connect).
export function subscribe(kind: string, fn: Listener): () => void {
  let set = listeners.get(kind);
  if (!set) {
    set = new Set();
    listeners.set(kind, set);
    attachKind(kind);
  }
  set.add(fn);
  return () => set.delete(fn);
}
