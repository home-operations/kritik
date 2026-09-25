<script lang="ts">
  import { href } from '../router.svelte';
  import { Paged, live } from '../resource.svelte';
  import { indexTone, splitRepo } from '../format';
  import type { Repository } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import ReviewStatusPill from '../components/ReviewStatusPill.svelte';
  import LoadMore from '../components/LoadMore.svelte';

  let { slug }: { slug: string } = $props();
  let filter = $state('');

  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/repos`);
  const paged = new Paged<Repository>(
    (cursor) => `${base}?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
    (r) => r.id,
  );
  const res = paged.first;

  $effect(() => {
    void paged.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind === 'index_run', () => void paged.load()));

  function visible(): Repository[] {
    const needle = filter.trim().toLowerCase();
    const all = paged.items;
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
      {#snippet children()}
        {@const rows = visible()}
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
        <LoadMore {paged} />
      {/snippet}
    </StateView>
  </div>
</main>
