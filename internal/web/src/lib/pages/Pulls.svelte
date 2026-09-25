<script lang="ts">
  import { tick } from 'svelte';
  import { getJSON } from '../api.svelte';
  import { navigate } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { pullRoute } from '../links';
  import { listKeys } from '../listkeys';
  import type { Page, Pull, Repository, ReviewStatus } from '../types';
  import StateView from '../components/StateView.svelte';
  import PullRows from '../components/PullRows.svelte';

  let { slug }: { slug: string } = $props();

  const OUTCOMES: readonly ReviewStatus[] = ['running', 'prepared', 'completed', 'superseded', 'skipped', 'capped', 'failed', 'canceled'];

  let prState = $state<'open' | 'closed' | 'all'>('open');
  let outcome = $state<ReviewStatus | ''>('');
  let repoName = $state('');
  let qInput = $state('');
  let q = $state('');
  let extra = $state<Pull[]>([]);
  let cursor = $state<string | null>(null);
  let loadingMore = $state(false);
  let moreError = $state('');
  let selected = $state(-1);
  let searchEl = $state<HTMLInputElement | undefined>(undefined);

  const tenant = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}`);

  function query(after?: string): string {
    const p = new URLSearchParams({ state: prState, limit: '50' });
    if (outcome) p.set('outcome', outcome);
    if (repoName) p.set('repo', repoName);
    if (q) p.set('q', q);
    if (after) p.set('cursor', after);
    return `${tenant}/pulls?${p}`;
  }

  const res = new Resource(() => getJSON<Page<Pull>>(query()));
  const repos = new Resource(() => getJSON<Page<Repository>>(`${tenant}/repos?limit=100`));

  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void repos.load();
  });
  $effect(() => live((e) => e.tenant === slug && (e.kind === 'review' || e.kind === 'followup'), () => void res.load()));
  $effect(() => {
    cursor = res.data?.nextCursor ?? null;
    extra = [];
    moreError = '';
  });
  // Debounce the search box so typing doesn't fire a request per keystroke.
  $effect(() => {
    const v = qInput.trim();
    const t = setTimeout(() => (q = v), 250);
    return () => clearTimeout(t);
  });

  const items = $derived([...(res.data?.items ?? []), ...extra]);

  async function more(): Promise<void> {
    if (!cursor) return;
    loadingMore = true;
    moreError = '';
    try {
      const p = await getJSON<Page<Pull>>(query(cursor));
      extra = [...extra, ...p.items];
      cursor = p.nextCursor;
    } catch (err) {
      moreError = err instanceof Error ? err.message : String(err);
    } finally {
      loadingMore = false;
    }
  }

  async function select(i: number): Promise<void> {
    selected = i;
    await tick();
    document.querySelector(`.pull-rows [data-index="${i}"]`)?.scrollIntoView({ block: 'nearest' });
  }

  $effect(() =>
    listKeys({
      count: () => items.length,
      get: () => selected,
      set: (i) => void select(i),
      open: (i) => {
        const p = items[i];
        if (p) navigate(pullRoute(slug, p));
      },
      focusSearch: () => searchEl?.focus(),
    }),
  );
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Pull requests</h1>
      <p class="muted small"><kbd>j</kbd>/<kbd>k</kbd> move · <kbd>⏎</kbd> open · <kbd>/</kbd> search</p>
    </header>
    <div class="toolbar" role="search">
      <label class="search-box">
        <span class="sr-only">Search pull requests</span>
        <input type="search" placeholder="Search title, author, number" bind:value={qInput} bind:this={searchEl} />
      </label>
      <label class="select">
        <span class="sr-only">Repository</span>
        <select bind:value={repoName} aria-label="Repository">
          <option value="">All repositories</option>
          {#each repos.data?.items ?? [] as r (r.id)}
            <option value={r.fullName}>{r.fullName}</option>
          {/each}
        </select>
      </label>
      <label class="select">
        <span class="sr-only">State</span>
        <select bind:value={prState} aria-label="State">
          <option value="open">Open</option>
          <option value="closed">Closed</option>
          <option value="all">All</option>
        </select>
      </label>
      <label class="select">
        <span class="sr-only">Last review outcome</span>
        <select bind:value={outcome} aria-label="Last review outcome">
          <option value="">Any outcome</option>
          {#each OUTCOMES as o (o)}<option value={o}>{o}</option>{/each}
        </select>
      </label>
    </div>
    <StateView {res} retry={() => res.load()} isEmpty={() => items.length === 0} empty="No pull requests match.">
      {#snippet children()}
        <PullRows {slug} {items} {selected} />
        {#if moreError}<p class="state-msg state-error" role="alert">{moreError}</p>{/if}
        {#if cursor}
          <button class="btn load-more" onclick={more} disabled={loadingMore}>{loadingMore ? 'Loading…' : 'Load more'}</button>
        {/if}
      {/snippet}
    </StateView>
  </div>
</main>
