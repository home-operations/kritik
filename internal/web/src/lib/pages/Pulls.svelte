<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { getJSON } from '../api.svelte';
  import { navigate, replace } from '../router.svelte';
  import { pullFilter, type PullFilter } from '../routes';
  import { Paged, Resource, live } from '../resource.svelte';
  import { pullRoute, accountApi } from '../links';
  import { listKeys } from '../listkeys';
  import { parseSearch, formatSearch, suggest, complete, type Suggestion } from '../pullquery';
  import type { Page, Pull, Repository } from '../types';
  import StateView from '../components/StateView.svelte';
  import PullTable from '../components/PullTable.svelte';
  import LoadMore from '../components/LoadMore.svelte';
  import SectionTabs from '../components/SectionTabs.svelte';
  import Segmented from '../components/Segmented.svelte';

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

  // The search box reaches the URL once typing pauses, so typing doesn't
  // fire a request per keystroke. written is the box's text for the filter
  // it last wrote there: any other change to the filter, such as following
  // a link to the unfiltered list, rewrites the box.
  let text = $state(untrack(() => formatSearch(filter)));
  let written = untrack(() => formatSearch(filter));
  let pending: ReturnType<typeof setTimeout> | undefined;

  function apply(): void {
    clearTimeout(pending);
    pending = setTimeout(() => {
      const { fields } = parseSearch(text, repoNames);
      written = formatSearch(fields);
      setFilter({ state: filter?.state, ...fields });
    }, 250);
  }
  $effect(() => () => clearTimeout(pending));
  $effect(() => {
    const f = formatSearch(filter);
    if (f === written) return;
    written = f;
    text = f;
  });

  // unknown names the tokens the list is not filtered by, once the
  // repositories they are checked against have loaded.
  const unknown = $derived(repos.data ? parseSearch(text, repoNames).unknown : []);

  // The box suggests the token being typed and its values: a combobox whose
  // list opens while the box has focus and something matches.
  let focused = $state(false);
  let dismissed = $state(false);
  let active = $state(-1);
  const authors = $derived([...new Set(paged.items.map((p) => p.author))].sort());
  const suggestions = $derived(suggest(text, { repos: repoNames, authors }));
  const listOpen = $derived(focused && !dismissed && suggestions.length > 0);

  function accept(s: Suggestion): void {
    text = complete(text, s);
    active = -1;
    apply();
    searchEl?.focus();
  }

  function onSearchKeydown(e: KeyboardEvent): void {
    if (!listOpen) return;
    const n = suggestions.length;
    if (e.key === 'ArrowDown') {
      active = (active + 1) % n;
    } else if (e.key === 'ArrowUp') {
      active = active <= 0 ? n - 1 : active - 1;
    } else if (e.key === 'Enter' && active >= 0) {
      accept(suggestions[active]!);
    } else if (e.key === 'Tab' && !e.shiftKey) {
      accept(suggestions[Math.max(active, 0)]!);
    } else if (e.key === 'Escape') {
      dismissed = true;
      e.stopPropagation();
    } else {
      return;
    }
    e.preventDefault();
  }

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
      <div class="search-box search-combo">
        <label class="sr-only" for="pull-search">Search pull requests</label>
        <input
          id="pull-search"
          type="search"
          role="combobox"
          autocomplete="off"
          aria-autocomplete="list"
          aria-expanded={listOpen}
          aria-controls="pull-suggest"
          aria-activedescendant={listOpen && active >= 0 ? `pull-suggest-${active}` : undefined}
          placeholder="Search, or filter by repo:, author: or status:"
          bind:value={text}
          bind:this={searchEl}
          oninput={() => {
            dismissed = false;
            active = -1;
            apply();
          }}
          onfocus={() => (focused = true)}
          onblur={() => (focused = false)}
          onkeydown={onSearchKeydown}
        />
        {#if listOpen}
          <ul class="suggest" id="pull-suggest" role="listbox" aria-label="Suggestions">
            {#each suggestions as s, i (s.label)}
              <!-- The box keeps focus: the list is picked with the arrow keys, Enter or Tab, or the pointer. -->
              <!-- svelte-ignore a11y_click_events_have_key_events -->
              <li
                id="pull-suggest-{i}"
                role="option"
                aria-selected={i === active}
                class:active={i === active}
                onmousedown={(e) => e.preventDefault()}
                onclick={() => accept(s)}
              >
                <span class="mono">{s.label}</span>
                {#if s.hint}<span class="muted small">{s.hint}</span>{/if}
              </li>
            {/each}
          </ul>
        {/if}
      </div>
      <Segmented label="State" options={STATES} value={prState} onchange={(state) => setFilter({ ...filter, state })} />
    </div>
    {#if unknown.length}
      <p class="small muted" role="note">Not filtering by {unknown.join(', ')}: no such repository or status.</p>
    {/if}
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
