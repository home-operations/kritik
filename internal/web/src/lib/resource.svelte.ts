// Resource is one fetched value plus its loading/error state, so every page
// renders the same loading, error and loaded states. A load that finishes
// after a newer one started is dropped, so a slow response can't overwrite
// a fresher one.
import { ApiError, getJSON } from './api.svelte';
import { subscribe } from './events.svelte';
import type { EventKind, LiveEvent, Page } from './types';

export class Resource<T> {
  data = $state<T | undefined>(undefined);
  error = $state<Error | undefined>(undefined);
  loading = $state(false);
  #seq = 0;
  readonly #fetcher: () => Promise<T>;

  constructor(fetcher: () => Promise<T>) {
    this.#fetcher = fetcher;
  }

  async load(): Promise<void> {
    const seq = ++this.#seq;
    this.loading = true;
    try {
      const data = await this.#fetcher();
      if (seq !== this.#seq) return;
      this.data = data;
      this.error = undefined;
    } catch (err) {
      if (seq !== this.#seq) return;
      this.error = err instanceof Error ? err : new Error(String(err));
    } finally {
      if (seq === this.#seq) this.loading = false;
    }
  }
}

export function errorMessage(err: Error): string {
  return err instanceof ApiError ? `${err.message} (${err.status})` : err.message;
}

const KINDS: readonly EventKind[] = ['review', 'runner_run', 'index_run', 'followup', 'model_call'];

// What a debounced refetch was woken by: the event kinds seen since the last
// refetch, plus 'resync' when the server asked for everything.
export type Dirty = ReadonlySet<EventKind | 'resync'>;

function isLiveEvent(v: unknown): v is LiveEvent {
  return typeof v === 'object' && v !== null && 'kind' in v && 'tenant' in v;
}

// live calls refetch whenever a server-sent event matches, and on every
// "resync" (the server lost track of what this client saw). Calls are
// debounced by delayMs, but a steady stream still refetches at least every
// maxWaitMs so the page never freezes mid-run. It returns the unsubscribe,
// so it drops straight into an $effect.
export function live(
  match: (e: LiveEvent) => boolean,
  refetch: (dirty: Dirty) => void,
  delayMs = 300,
  maxWaitMs = 3000,
): () => void {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let since = 0;
  let dirty = new Set<EventKind | 'resync'>();
  const run = () => {
    timer = undefined;
    const d = dirty;
    dirty = new Set();
    refetch(d);
  };
  const fire = (kind: EventKind | 'resync') => {
    dirty.add(kind);
    const now = Date.now();
    if (timer === undefined) since = now;
    else if (now - since >= maxWaitMs) return;
    else clearTimeout(timer);
    timer = setTimeout(run, Math.min(delayMs, Math.max(0, since + maxWaitMs - now)));
  };
  const offs = KINDS.map((k) =>
    subscribe(k, (d) => {
      if (isLiveEvent(d) && match(d)) fire(k);
    }),
  );
  offs.push(subscribe('resync', () => fire('resync')));
  return () => {
    clearTimeout(timer);
    for (const off of offs) off();
  };
}

// Paged is a keyset-paginated list: the first page is a Resource (so
// StateView renders it), and "Load more" appends further pages. Reloading
// the same query (a live refetch) refreshes the first page but keeps the
// pages already loaded; changing the query (url() returns a different base)
// drops them, and a Load more still in flight for the old query is
// discarded when it lands.
export class Paged<T> {
  readonly first: Resource<Page<T>>;
  extra = $state<T[]>([]);
  loadingMore = $state(false);
  moreError = $state<Error | undefined>(undefined);
  // The cursor after the last extra page; undefined until one is loaded.
  #after = $state<string | null | undefined>(undefined);
  #gen = 0;
  #base: string | undefined;
  readonly #url: (cursor?: string) => string;
  readonly #key: (t: T) => string;

  constructor(url: (cursor?: string) => string, key: (t: T) => string) {
    this.#url = url;
    this.#key = key;
    this.first = new Resource(() => getJSON<Page<T>>(url()));
  }

  get cursor(): string | null {
    return this.#after !== undefined ? this.#after : (this.first.data?.nextCursor ?? null);
  }

  get items(): T[] {
    const head = this.first.data?.items ?? [];
    const seen = new Set(head.map(this.#key));
    return [...head, ...this.extra.filter((t) => !seen.has(this.#key(t)))];
  }

  load(): Promise<void> {
    const base = this.#url();
    if (base !== this.#base) {
      this.#base = base;
      this.#gen++;
      this.extra = [];
      this.#after = undefined;
      this.moreError = undefined;
      this.loadingMore = false;
    }
    return this.first.load();
  }

  async more(): Promise<void> {
    const cursor = this.cursor;
    if (!cursor) return;
    const gen = this.#gen;
    this.loadingMore = true;
    this.moreError = undefined;
    try {
      const p = await getJSON<Page<T>>(this.#url(cursor));
      if (gen !== this.#gen) return;
      const have = new Set(this.extra.map(this.#key));
      this.extra = [...this.extra, ...p.items.filter((t) => !have.has(this.#key(t)))];
      this.#after = p.nextCursor;
    } catch (err) {
      if (gen === this.#gen) this.moreError = err instanceof Error ? err : new Error(String(err));
    } finally {
      if (gen === this.#gen) this.loadingMore = false;
    }
  }
}
