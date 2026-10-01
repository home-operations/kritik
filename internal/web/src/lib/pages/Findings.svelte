<script lang="ts">
  // Every finding of the account, once per pull request however many of
  // its reviews repeated it, and whether a later review found it addressed
  // or a maintainer dismissed it.
  import { untrack } from 'svelte';
  import { getJSON } from '../api.svelte';
  import { href, navigate, replace } from '../router.svelte';
  import { FINDING_STATUSES, findingFilter, type FindingFilter } from '../routes';
  import { Paged, Resource, live } from '../resource.svelte';
  import { accountApi, pullRoute, threadUrl } from '../links';
  import { CATEGORIES, SEVERITIES } from '../format';
  import { formatTokens, type Parsed, type TokenSpec } from '../tokensearch';
  import type { AccountFinding, Page, Repository } from '../types';
  import StateView from '../components/StateView.svelte';
  import LoadMore from '../components/LoadMore.svelte';
  import SectionTabs from '../components/SectionTabs.svelte';
  import TokenSearch from '../components/TokenSearch.svelte';
  import Reactions from '../components/Reactions.svelte';
  import Time from '../components/Time.svelte';
  import Icon from '../Icon.svelte';
  import { mdiCancel, mdiCheck, mdiCircleOutline, mdiOpenInNew } from '../icons';

  const statusIcon = { open: mdiCircleOutline, addressed: mdiCheck, dismissed: mdiCancel } as const;

  let { slug, filter }: { slug: string; filter?: FindingFilter } = $props();

  const account = $derived(accountApi(slug));
  function query(after?: string): string {
    const p = new URLSearchParams({ limit: '50' });
    for (const k of ['severity', 'category', 'status', 'repo', 'rule', 'q'] as const) {
      const v = filter?.[k];
      if (v) p.set(k, v);
    }
    if (after) p.set('cursor', after);
    return `${account}/findings?${p}`;
  }
  const paged = new Paged<AccountFinding>(query, (f) => f.id);
  const res = paged.first;
  const repos = new Resource(() => getJSON<Page<Repository>>(`${account}/repos?limit=100`));
  $effect(() => {
    void paged.load();
  });
  $effect(() => {
    void repos.load();
  });
  $effect(() => live((e) => e.account === slug && e.kind === 'review', () => void paged.load()));

  const specs = $derived<TokenSpec[]>([
    { key: 'repo', hint: 'a repository', values: (repos.data?.items ?? []).map((r) => r.fullName) },
    { key: 'severity', hint: 'blocking, important or nit', values: SEVERITIES },
    { key: 'category', hint: 'what kind of problem', values: CATEGORIES },
    { key: 'status', hint: 'open, addressed or dismissed', values: FINDING_STATUSES },
    { key: 'rule', hint: 'a rule id', values: [...new Set(paged.items.flatMap((f) => f.rules))].sort(), open: true },
  ]);
  const boxText = (f: FindingFilter | undefined) =>
    formatTokens(specs, { repo: f?.repo, severity: f?.severity, category: f?.category, status: f?.status, rule: f?.rule }, f?.q);

  // As on the pull request list: written is the box's text for the filter it
  // last wrote to the URL, and any other change to the filter rewrites it.
  let searchEl = $state<HTMLInputElement | undefined>(undefined);
  let text = $state(untrack(() => boxText(filter)));
  let written = untrack(() => boxText(filter));
  function onapply(p: Parsed): void {
    const f = findingFilter({ ...p.tokens, q: p.q });
    written = boxText(f);
    replace({ name: 'findings', slug, filter: f });
  }
  $effect(() => {
    const t = boxText(filter);
    if (t === written) return;
    written = t;
    text = t;
  });

  function clearFilters(): void {
    replace({ name: 'findings', slug });
    searchEl?.focus();
  }

  const reviewOf = (f: AccountFinding) => ({ name: 'review' as const, slug, id: f.reviewId });

  function onRowClick(e: MouseEvent, f: AccountFinding): void {
    if ((e.target as Element).closest('a, button') || getSelection()?.toString()) return;
    navigate(reviewOf(f));
  }
</script>

<svelte:head><title>Findings · {slug} · kritika</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <SectionTabs section="analytics" {slug} current="findings" />
    <div class="toolbar" role="search">
      <TokenSearch
        id="finding-search"
        label="Search findings"
        placeholder="Search, or filter by repo:, severity:, category:, status: or rule:"
        {specs}
        bind:text
        bind:input={searchEl}
        ready={!!repos.data}
        {onapply}
      />
    </div>
    <StateView {res} retry={() => res.load()}>
      {#snippet children()}
        {#if paged.items.length === 0 && filter}
          <div class="state-msg">
            <span>No findings match these filters.</span>
            <button class="btn btn-small" onclick={clearFilters}>Clear filters</button>
          </div>
        {:else if paged.items.length === 0}
          <p class="state-msg">No findings yet: they appear here as reviews report them.</p>
        {:else}
          <div class="table-wrap table-card">
            <table class="data finding-table">
              <thead>
                <tr>
                  <th scope="col">Finding</th>
                  <th scope="col">Severity · kind</th>
                  <th scope="col">Pull request</th>
                  <th scope="col" title="Addressed once a later review of the pull request no longer reports it; dismissed when a maintainer replied so in its thread">Status</th>
                  <th scope="col" class="num">Found</th>
                </tr>
              </thead>
              <tbody>
                {#each paged.items as f (f.id)}
                  <!-- The title is the row's link; the click is a larger target for a pointer. -->
                  <tr class="finding-row" onclick={(e) => onRowClick(e, f)}>
                    <td class="finding-main">
                      <a class="finding-link" href={href(reviewOf(f))}>{f.title}</a>
                      <span class="finding-sub">{f.explanation}</span>
                      {#if f.rules.length}
                        <span class="finding-rules">
                          {#each f.rules as id (id)}
                            <a class="mono" href={href({ name: 'findings', slug, filter: { rule: id } })} title="Findings that cite the rule {id}">{id}</a>
                          {/each}
                        </span>
                      {/if}
                    </td>
                    <td>
                      <span class="sev sev-{f.severity}">{f.severity}</span>
                      {#if f.category}<a class="badge" href={href({ name: 'findings', slug, filter: { category: f.category } })} title="Findings of this kind">{f.category}</a>{/if}
                    </td>
                    <td class="finding-pull">
                      <a href={href(pullRoute(slug, f.pull))} title={f.pull.title}><span class="mono">{f.pull.repository}</span> #{f.pull.number}</a>
                      <span class="finding-sub mono">
                        {f.path}:{f.line}
                        {#if threadUrl(f.pull.url, f.forgeCommentId)}
                          <a class="external" href={threadUrl(f.pull.url, f.forgeCommentId)} target="_blank" rel="noopener noreferrer" title="The finding's thread on GitHub">
                            <Icon path={mdiOpenInNew} size={11} label="Thread on GitHub" />
                          </a>
                        {/if}
                      </span>
                    </td>
                    <td>
                      <span class="status" class:tone-ok={f.status === 'addressed'} class:tone-muted={f.status !== 'addressed'} title={f.dismissReason}>
                        <span class="status-tile"><Icon path={statusIcon[f.status]} size={12} /></span>
                        <span class="status-word">{f.status}</span>
                      </span>
                      <Reactions up={f.reactionsUp} down={f.reactionsDown} />
                    </td>
                    <td class="num"><Time iso={f.firstSeenAt} /></td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
          <LoadMore {paged} />
        {/if}
      {/snippet}
    </StateView>
  </div>
</main>
