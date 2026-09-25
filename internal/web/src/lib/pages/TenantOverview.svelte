<script lang="ts">
  import { untrack } from 'svelte';
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live, type Dirty } from '../resource.svelte';
  import { tokens, usd, wholeNumber, indexTone, jobTone, splitRepo } from '../format';
  import type { Job, JobState, Page, Pull, Repository, TenantSummary } from '../types';
  import StateView from '../components/StateView.svelte';
  import Meter from '../components/Meter.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import ReviewStatusPill from '../components/ReviewStatusPill.svelte';
  import SeverityCounts from '../components/SeverityCounts.svelte';

  let { slug }: { slug: string } = $props();

  interface Data {
    summary: TenantSummary | undefined;
    repos: Page<Repository>;
    open: Page<Pull>;
    recent: Pull[];
    queue: Job[];
  }

  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}`);

  type Part = 'summary' | 'repos' | 'pulls' | 'queue';
  const ALL: readonly Part[] = ['summary', 'repos', 'pulls', 'queue'];
  // Which parts the next load refetches; the rest are reused from the last
  // load, so one live event costs one or two requests rather than five.
  let stale = new Set<Part>(ALL);

  function partsFor(dirty: Dirty): Part[] {
    if (dirty.has('resync')) return [...ALL];
    const out = new Set<Part>();
    if (dirty.has('review')) ['summary', 'pulls', 'queue'].forEach((p) => out.add(p as Part));
    if (dirty.has('index_run')) ['repos', 'queue'].forEach((p) => out.add(p as Part));
    if (dirty.has('runner_run') || dirty.has('followup')) out.add('queue');
    return [...out];
  }

  const res: Resource<Data> = new Resource<Data>(async (): Promise<Data> => {
    const b = base;
    // untrack: the load effect must not re-run just because data arrived.
    const prev: Data | undefined = untrack(() => res.data);
    const want = prev ? stale : new Set(ALL);
    stale = new Set();
    const [tenants, repos, pulls, queue] = await Promise.all([
      want.has('summary') || !prev ? getJSON<TenantSummary[]>('/api/v1/tenants') : undefined,
      want.has('repos') || !prev ? getJSON<Page<Repository>>(`${b}/repos?limit=100`) : prev.repos,
      want.has('pulls') || !prev
        ? Promise.all([getJSON<Page<Pull>>(`${b}/pulls?state=open&limit=100`), getJSON<Page<Pull>>(`${b}/pulls?state=all&limit=50`)])
        : undefined,
      want.has('queue') || !prev ? getJSON<Job[]>(`${b}/queue`) : prev.queue,
    ]);
    const recent = pulls
      ? pulls[1].items
          .filter((p) => p.lastReview)
          .sort((a, z) => (z.lastReview?.createdAt ?? '').localeCompare(a.lastReview?.createdAt ?? ''))
          .slice(0, 10)
      : prev!.recent;
    return {
      summary: tenants ? tenants.find((t) => t.slug === slug) : prev?.summary,
      repos,
      open: pulls ? pulls[0] : prev!.open,
      recent,
      queue,
    };
  });

  $effect(() => {
    void res.load();
  });
  // model_call events are ignored: nothing here changes per model call
  // except the token tile, which the next review event refreshes.
  $effect(() =>
    live(
      (e) => e.tenant === slug && e.kind !== 'model_call',
      (dirty) => {
        const parts = partsFor(dirty);
        if (!parts.length) return;
        for (const p of parts) stale.add(p);
        void res.load();
      },
    ),
  );

  const INFLIGHT: readonly JobState[] = ['running', 'available', 'scheduled', 'retryable', 'pending'];

  function queueCounts(jobs: Job[]): { state: JobState; n: number }[] {
    return INFLIGHT.map((state) => ({ state, n: jobs.filter((j) => j.state === state).length })).filter((c) => c.n > 0);
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1><span class="mono">{slug}</span> overview</h1></header>
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        <section class="tiles" aria-label="At a glance">
          <div class="tile">
            <span class="tile-label">Reviews, last 7 days</span>
            <span class="tile-value">{d.summary ? wholeNumber(d.summary.reviews7d) : '—'}</span>
            {#if d.summary}
              <span class="small muted">{d.summary.usage.reviewsToday} today{d.summary.usage.reviewsPerDay ? ` of ${d.summary.usage.reviewsPerDay}` : ''}</span>
            {/if}
          </div>
          <div class="tile">
            <span class="tile-label">Open pull requests</span>
            <span class="tile-value">{d.open.items.length}{d.open.nextCursor ? '+' : ''}</span>
          </div>
          <div class="tile">
            <span class="tile-label">Spend this month</span>
            <span class="tile-value">{d.summary ? usd(d.summary.usage.costUsd) : '—'}</span>
          </div>
          <div class="tile">
            <span class="tile-label">Tokens this month</span>
            {#if d.summary}
              <span class="tile-value" title={wholeNumber(d.summary.usage.tokens)}>{tokens(d.summary.usage.tokens)}</span>
              <span class="small muted">
                {d.summary.usage.tokensPerMonth ? `of ${tokens(d.summary.usage.tokensPerMonth)} cap` : 'no monthly cap'}
              </span>
              <Meter value={d.summary.usage.tokens} max={d.summary.usage.tokensPerMonth} label="Monthly tokens used" />
            {:else}
              <span class="tile-value">—</span>
            {/if}
          </div>
        </section>

        <div class="grid-2">
          <section class="panel" aria-labelledby="ov-recent">
            <header class="panel-head">
              <h2 id="ov-recent">Recent reviews</h2>
              <a class="small" href={href({ name: 'pulls', slug })}>All pulls</a>
            </header>
            {#if d.recent.length === 0}
              <p class="state-msg">No reviews yet.</p>
            {:else}
              <ul class="rows">
                {#each d.recent as p (`${p.repository}#${p.number}`)}
                  {@const r = p.lastReview!}
                  <li class="row">
                    <a class="row-link" href={href({ name: 'review', slug, id: r.id })}>
                      <span class="mono small muted">{p.repository}#{p.number}</span>
                      <span class="row-text">{p.title}</span>
                    </a>
                    <span class="row-meta">
                      <SeverityCounts counts={r.findings} />
                      <ReviewStatusPill status={r.status} />
                      <Time iso={r.createdAt} />
                    </span>
                  </li>
                {/each}
              </ul>
            {/if}
          </section>

          <section class="panel" aria-labelledby="ov-queue">
            <header class="panel-head">
              <h2 id="ov-queue">In flight</h2>
              <a class="small" href={href({ name: 'queue', slug })}>Queue</a>
            </header>
            {#if queueCounts(d.queue).length === 0}
              <p class="state-msg">Nothing queued.</p>
            {:else}
              <ul class="chips">
                {#each queueCounts(d.queue) as c (c.state)}
                  <li><Pill tone={jobTone[c.state]} label="{c.n} {c.state}" /></li>
                {/each}
              </ul>
            {/if}
          </section>
        </div>

        <section class="panel" aria-labelledby="ov-repos">
          <header class="panel-head">
            <h2 id="ov-repos">Repositories</h2>
            <a class="small" href={href({ name: 'repos', slug })}>All repositories</a>
          </header>
          {#if d.repos.items.length === 0}
            <p class="state-msg">No repositories yet.</p>
          {:else}
            <div class="table-wrap">
              <table class="data">
                <thead>
                  <tr><th scope="col">Repository</th><th scope="col">Index</th><th scope="col">Indexed commit</th><th scope="col">Last review</th></tr>
                </thead>
                <tbody>
                  {#each d.repos.items as repo (repo.id)}
                    {@const n = splitRepo(repo.fullName)}
                    <tr>
                      <td class="mono"><a href={href({ name: 'repo', slug, owner: n.owner, repo: n.repo })}>{repo.fullName}</a></td>
                      <td>
                        {#if repo.index.lastRunStatus}
                          <Pill tone={indexTone[repo.index.lastRunStatus]} label={repo.index.lastRunStatus} />
                        {:else}<span class="muted">never</span>{/if}
                      </td>
                      <td class="mono small">{repo.index.activeCommit.slice(0, 7) || '—'} <Time iso={repo.index.activeAt} /></td>
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
        </section>
      {/snippet}
    </StateView>
  </div>
</main>
