// Resource is one fetched value plus its loading/error state, so every page
// renders the same loading, error and loaded states. A load that finishes
// after a newer one started is dropped, so a slow response can't overwrite
// a fresher one.
import { ApiError } from './api.svelte';
import { subscribe } from './events.svelte';
import type { EventKind, LiveEvent } from './types';

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

function isLiveEvent(v: unknown): v is LiveEvent {
  return typeof v === 'object' && v !== null && 'kind' in v && 'tenant' in v;
}

// live calls refetch, debounced, whenever a server-sent event matches, and
// on every "resync" (the server lost track of what this client saw). It
// returns the unsubscribe, so it drops straight into an $effect.
export function live(match: (e: LiveEvent) => boolean, refetch: () => void, delayMs = 300): () => void {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const fire = () => {
    clearTimeout(timer);
    timer = setTimeout(refetch, delayMs);
  };
  const offs = KINDS.map((k) =>
    subscribe(k, (d) => {
      if (isLiveEvent(d) && match(d)) fire();
    }),
  );
  offs.push(subscribe('resync', fire));
  return () => {
    clearTimeout(timer);
    for (const off of offs) off();
  };
}
