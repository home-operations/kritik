<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { getJSON } from '../api.svelte';
  import { navigate, replace } from '../router.svelte';
  import { PULL_OUTCOMES, pullFilter, type PullFilter } from '../routes';
  import { Paged, Resource, live } from '../resource.svelte';
  import { pullRoute, accountApi } from '../links';
  import { listKeys } from '../listkeys';
  import { formatTokens, type Parsed, type TokenSpec } from '../tokensearch';
  import type { Page, Pull, Repository } from '../types';
  import StateView from '../components/StateView.svelte';
  import PullTable from '../components/PullTable.svelte';
  import LoadMore from '../components/LoadMore.svelte';
  import SectionTabs from '../components/SectionTabs.svelte';
  import Segmented from '../components/Segmented.svelte';
  import TokenSearch from '../components/TokenSearch.svelte';

  let { slug, filter }: { slug: string; filter?: PullFilter } = $props();

  const prState = $derived(filter?.state ?? 'open');
  let searchEl = $state<HTMLInputElement | undefined>(undefined);

  // setFilter replaces the history entry, so Back leaves the list instead of
  // stepping through its filters.
  function setFilter(f: Parameters<typeof pullFilter>[0]): void {
    replace({ name: 'pulls', slug, filter: pullFilter(f) });
  }

  function clearFilters(): void {
    replace({ name: 'pulls', slug });
    searchEl?.focus();
  }

  const account = $derived(`${accountApi(slug)}`);

  function query(after?: string): string {
    const p = new URLSearchParams({ state: prState, limit: '50' });
    for (const k of ['outcome', 'repo', 'author', 'q'] as const) {
      const v = filter?.[k];
      if (v) p.set(k, v);
    }
    if (after) p.set('cursor', after);
    return `${account}/pulls?${p}`;
  }

  const pullKey = (p: Pull) => `${p.repository}#${p.number}`;
  const paged = new Paged<Pull>(query, pullKey);
  const res = paged.first;
  const repos = new Resource(() => getJSON<Page<Repository>>(`${account}/repos?limit=100`));
  const repoNames = $derived((repos.data?.items ?? []).map((r) => r.fullName));

  $effect(() => {
    void paged.load();
  });
  $effect(() => {
    void repos.load();
  });
  $effect(() => live((e) => e.account === slug && (e.kind === 'review' || e.kind === 'followup'), () => void paged.load()));

  // The box's tokens: status: is the last review's outcome.
  const authors = $derived([...new Set(paged.items.map((p) => p.author))].sort());
  const specs = $derived<TokenSpec[]>([
    { key: 'repo', hint: 'a repository', values: repoNames },
    { key: 'author', hint: "an author's login", values: authors, open: true },
    { key: 'status', hint: "the last review's status", values: PULL_OUTCOMES },
  ]);
  const boxText = (f: PullFilter | undefined) => formatTokens(specs, { repo: f?.repo, author: f?.author, status: f?.outcome }, f?.q);

  // written is the box's text for the filter it last wrote to the URL: any
  // other change to the filter, such as following a link to the unfiltered
  // list, rewrites the box.
  let text = $state(untrack(() => boxText(filter)));
  let written = untrack(() => boxText(filter));
  function onapply(p: Parsed): void {
    const f = { state: filter?.state, repo: p.tokens.repo, author: p.tokens.author, outcome: p.tokens.status, q: p.q };
    written = boxText(pullFilter(f));
    setFilter(f);
  }
  $effect(() => {
    const t = boxText(filter);
    if (t === written) return;
    written = t;
    text = t;
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

  const STATES = [
    { value: 'open', label: 'Open' },
    { value: 'closed', label: 'Closed' },
    { value: 'all', label: 'All' },
  ] as const;
</script>

<svelte:head><title>Pull requests · {slug} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <SectionTabs section="pulls" {slug} current="pulls" />
    <div class="toolbar" role="search">
      <TokenSearch
        id="pull-search"
        label="Search pull requests"
        placeholder="Search, or filter by repo:, author: or status:"
        {specs}
        bind:text
        bind:input={searchEl}
        ready={!!repos.data}
        {onapply}
      />
      <Segmented label="State" options={STATES} value={prState} onchange={(state) => setFilter({ ...filter, state })} />
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
          <PullTable {slug} {items} {selected} />
          <LoadMore {paged} />
        {/if}
      {/snippet}
    </StateView>
    <p class="muted small key-hints"><kbd>j</kbd>/<kbd>k</kbd> move · <kbd>⏎</kbd> open · <kbd>/</kbd> search</p>
  </div>
</main>
