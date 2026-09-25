<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { indexTone, splitRepo } from '../format';
  import type { Page, Repository } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import ReviewStatusPill from '../components/ReviewStatusPill.svelte';

  let { slug }: { slug: string } = $props();
  let filter = $state('');
  let extra = $state<Repository[]>([]);
  let cursor = $state<string | null>(null);
  let loadingMore = $state(false);

  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/repos`);
  const res = new Resource(() => getJSON<Page<Repository>>(`${base}?limit=100`));

  $effect(() => {
    void res.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind === 'index_run', () => void res.load()));
  $effect(() => {
    cursor = res.data?.nextCursor ?? null;
    extra = [];
  });

  async function more(): Promise<void> {
    if (!cursor) return;
    loadingMore = true;
    try {
      const p = await getJSON<Page<Repository>>(`${base}?limit=100&cursor=${encodeURIComponent(cursor)}`);
      extra = [...extra, ...p.items];
      cursor = p.nextCursor;
    } catch (err) {
      console.error('load more repos:', err);
    } finally {
      loadingMore = false;
    }
  }

  function visible(items: Repository[]): Repository[] {
    const needle = filter.trim().toLowerCase();
    const all = [...items, ...extra];
    return needle ? all.filter((r) => r.fullName.toLowerCase().includes(needle) || r.installation.toLowerCase().includes(needle)) : all;
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Repositories</h1></header>
    <div class="toolbar">
      <label class="search-box">
        <span class="sr-only">Filter repositories</span>
        <input type="search" placeholder="Filter by name or installation" bind:value={filter} />
      </label>
    </div>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.items.length === 0} empty="No repositories yet.">
      {#snippet children(d)}
        {@const rows = visible(d.items)}
        {#if rows.length === 0}
          <p class="state-msg">Nothing matches “{filter}”.</p>
        {:else}
          <div class="table-wrap">
            <table class="data">
              <thead>
                <tr>
                  <th scope="col">Repository</th>
                  <th scope="col">Installation</th>
                  <th scope="col">Enabled</th>
                  <th scope="col">Index</th>
                  <th scope="col">Indexed commit</th>
                  <th scope="col">Last review</th>
                </tr>
              </thead>
              <tbody>
                {#each rows as repo (repo.id)}
                  {@const n = splitRepo(repo.fullName)}
                  <tr>
                    <td class="mono"><a href={href({ name: 'repo', slug, owner: n.owner, repo: n.repo })}>{repo.fullName}</a></td>
                    <td class="mono small">{repo.installation}</td>
                    <td>{#if repo.enabled}<Pill tone="ok" label="on" />{:else}<Pill label="off" />{/if}</td>
                    <td>
                      {#if repo.index.lastRunStatus}
                        <Pill tone={indexTone[repo.index.lastRunStatus]} label={repo.index.lastRunStatus} /> <Time iso={repo.index.lastRunAt} />
                      {:else}<span class="muted">never</span>{/if}
                    </td>
                    <td class="mono small">{repo.index.activeCommit.slice(0, 7) || '—'}</td>
                    <td>
                      {#if repo.lastReview}
                        <a href={href({ name: 'review', slug, id: repo.lastReview.id })}><ReviewStatusPill status={repo.lastReview.status} /></a>
                        <Time iso={repo.lastReview.createdAt} />
                      {:else}<span class="muted">—</span>{/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
        {#if cursor}
          <button class="btn load-more" onclick={more} disabled={loadingMore}>{loadingMore ? 'Loading…' : 'Load more'}</button>
        {/if}
      {/snippet}
    </StateView>
  </div>
</main>
