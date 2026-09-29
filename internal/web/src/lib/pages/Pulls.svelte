<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { getJSON } from '../api.svelte';
  import { navigate, replace } from '../router.svelte';
  import { PULL_OUTCOMES, pullFilter, type PullFilter } from '../routes';
  import { Paged, Resource, live } from '../resource.svelte';
  import { pullRoute, accountApi } from '../links';
  import { listKeys } from '../listkeys';
  import type { Page, Pull, Repository } from '../types';
  import StateView from '../components/StateView.svelte';
  import PullRows from '../components/PullRows.svelte';
  import LoadMore from '../components/LoadMore.svelte';

  let { slug, filter }: { slug: string; filter?: PullFilter } = $props();

  const prState = $derived(filter?.state ?? 'open');
  const outcome = $derived(filter?.outcome ?? '');
  const repoName = $derived(filter?.repo ?? '');
  const q = $derived(filter?.q ?? '');
  let searchEl = $state<HTMLInputElement | undefined>(undefined);

  // setFilter changes one filter in the URL, replacing the history entry so
  // Back leaves the list instead of stepping through its filters.
  function setFilter(change: { state?: string; outcome?: string; repo?: string; q?: string }): void {
    replace({ name: 'pulls', slug, filter: pullFilter({ ...filter, ...change }) });
  }

  function clearFilters(): void {
    replace({ name: 'pulls', slug });
    searchEl?.focus();
  }

  const account = $derived(`${accountApi(slug)}`);

  function query(after?: string): string {
    const p = new URLSearchParams({ state: prState, limit: '50' });
    if (outcome) p.set('outcome', outcome);
    if (repoName) p.set('repo', repoName);
    if (q) p.set('q', q);
    if (after) p.set('cursor', after);
    return `${account}/pulls?${p}`;
  }

  const pullKey = (p: Pull) => `${p.repository}#${p.number}`;
  const paged = new Paged<Pull>(query, pullKey);
  const res = paged.first;
  const repos = new Resource(() => getJSON<Page<Repository>>(`${account}/repos?limit=100`));

  $effect(() => {
    void paged.load();
  });
  $effect(() => {
    void repos.load();
  });
  $effect(() => live((e) => e.account === slug && (e.kind === 'review' || e.kind === 'followup'), () => void paged.load()));
  // The search box reaches the URL once typing pauses, so typing doesn't
  // fire a request per keystroke. sent is the last search it wrote there:
  // any other change to q, such as following a link to the unfiltered list,
  // replaces the box's text.
  let qInput = $state(untrack(() => q));
  let sent = untrack(() => q);
  $effect(() => {
    const v = qInput.trim();
    if (v === sent) return;
    const t = setTimeout(() => {
      sent = v;
      setFilter({ q: v });
    }, 250);
    return () => clearTimeout(t);
  });
  $effect(() => {
    if (q === sent) return;
    sent = q;
    qInput = q;
  });

  const items = $derived(paged.items);
  // The cursor follows its pull rather than its position, which a live
  // refetch shifts when it adds or reorders rows above it.
  let selectedKey = $state('');
  const selected = $derived(items.findIndex((p) => pullKey(p) === selectedKey));

  async function select(i: number): Promise<void> {
    const p = items[i];
    if (!p) return;
    selectedKey = pullKey(p);
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
        <select bind:value={() => repoName, (repo) => setFilter({ repo })} aria-label="Repository">
          <option value="">All repositories</option>
          {#each repos.data?.items ?? [] as r (r.id)}
            <option value={r.fullName}>{r.fullName}</option>
          {/each}
        </select>
      </label>
      <label class="select">
        <span class="sr-only">State</span>
        <select bind:value={() => prState, (state) => setFilter({ state })} aria-label="State">
          <option value="open">Open</option>
          <option value="closed">Closed</option>
          <option value="all">All</option>
        </select>
      </label>
      <label class="select">
        <span class="sr-only">Last review outcome</span>
        <select bind:value={() => outcome, (outcome) => setFilter({ outcome })} aria-label="Last review outcome">
          <option value="">Any outcome</option>
          {#each PULL_OUTCOMES as o (o)}<option value={o}>{o}</option>{/each}
        </select>
      </label>
    </div>
    <StateView {res} retry={() => res.load()}>
      {#snippet children()}
        {#if items.length === 0 && filter}
          <div class="state-msg">
            <span>No pull requests match these filters.</span>
            <button class="btn btn-small" onclick={clearFilters}>Clear filters</button>
          </div>
        {:else if items.length === 0}
          <p class="state-msg">No open pull requests.</p>
        {:else}
          <PullRows {slug} {items} {selected} />
          <LoadMore {paged} />
        {/if}
      {/snippet}
    </StateView>
  </div>
</main>
