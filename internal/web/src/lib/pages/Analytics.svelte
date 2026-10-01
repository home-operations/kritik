<script lang="ts">
  // The account's home: what its reviews came to over a window, against the
  // window before, and what needs attention now.
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { hookURL } from '../session.svelte';
  import { unsignedWebhooks } from '../setup';
  import { Resource, live } from '../resource.svelte';
  import { accountApi, repoRoute } from '../links';
  import { daysAgo, duration, reviewTone, usd, wholeNumber } from '../format';
  import type { AccountDetail, Analytics, AnalyticsPoint, Page, Pull } from '../types';
  import StateView from '../components/StateView.svelte';
  import SectionTabs from '../components/SectionTabs.svelte';
  import Segmented from '../components/Segmented.svelte';
  import StatTile from '../components/StatTile.svelte';
  import ColumnChart from '../components/ColumnChart.svelte';
  import SeverityCounts from '../components/SeverityCounts.svelte';
  import Pill from '../components/Pill.svelte';

  let { slug }: { slug: string } = $props();
  const base = $derived(accountApi(slug));

  const PERIODS = [
    { value: '7', label: '7 days' },
    { value: '30', label: '30 days' },
    { value: '90', label: '90 days' },
  ] as const;
  let days = $state<'7' | '30' | '90'>('30');
  // A quarter reads better by week than as ninety thin columns.
  const group = $derived(days === '90' ? 'week' : 'day');

  const res = new Resource(() => {
    const p = new URLSearchParams({ from: daysAgo(Number(days), Date.now()), group });
    return getJSON<Analytics>(`${base}/analytics?${p}`);
  });
  const detail = new Resource(() => getJSON<AccountDetail>(base));
  const open = new Resource(() => getJSON<Page<Pull>>(`${base}/pulls?state=open&limit=100`));
  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void detail.load();
    void open.load();
  });
  $effect(() =>
    live(
      (e) => e.account === slug && e.kind === 'review',
      () => {
        void res.load();
        void open.load();
      },
      1000,
    ),
  );

  // The open pulls whose last review did not get done, by why. Only the
  // first page of open pulls is loaded, so while there are more a count is
  // a floor.
  const STUCK = [
    { outcome: 'failed', why: 'last review failed' },
    { outcome: 'capped', why: 'last review hit a limit' },
  ] as const;
  function stuck(p: Page<Pull>) {
    const more = p.nextCursor ? '+' : '';
    return STUCK.flatMap(({ outcome, why }) => {
      const n = p.items.filter((x) => x.lastReview?.status === outcome).length;
      return n ? [{ outcome, text: `${n}${more} open ${n === 1 && !more ? 'pull request' : 'pull requests'} whose ${why}` }] : [];
    });
  }

  const found = (c: { blocking: number; important: number; nit: number }) => c.blocking + c.important + c.nit;
  const rate = (addressed: number, total: number) => (total ? (addressed / total) * 100 : null);

  const dayFmt = new Intl.DateTimeFormat('en', { month: 'short', day: 'numeric', timeZone: 'UTC' });
  const when = (key: string) => dayFmt.format(new Date(`${key}T00:00:00Z`));
  function rows(series: AnalyticsPoint[], values: (p: AnalyticsPoint) => number[]) {
    return series.map((p) => ({
      key: p.key,
      label: when(p.key),
      title: group === 'week' ? `Week of ${when(p.key)}` : when(p.key),
      values: values(p),
    }));
  }

  let reviewsView = $state<'chart' | 'table'>('chart');
  let findingsView = $state<'chart' | 'table'>('chart');
  const VIEWS = [
    { value: 'chart', label: 'Chart' },
    { value: 'table', label: 'Table' },
  ] as const;
  const SEV_SERIES = [
    { label: 'Blocking', color: 'var(--chart-sev-blocking)' },
    { label: 'Important', color: 'var(--chart-sev-important)' },
    { label: 'Nit', color: 'var(--chart-sev-nit)' },
  ];
</script>

<svelte:head><title>Analytics · {slug} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <SectionTabs section="analytics" {slug} current="account" />
    {#if detail.data && (unsignedWebhooks(detail.data.connection) || !detail.data.connection.lastWebhookAt)}
      {@const inst = detail.data.connection}
      <section class="panel" aria-labelledby="an-connection">
        <header class="panel-head"><h2 id="an-connection">Connection</h2></header>
        {#if unsignedWebhooks(inst)}
          <p class="notice" role="note">
            GitHub sends <span class="mono">{inst.name}</span>'s webhooks with no signature, so kritik refuses them, only polls for
            new pull requests and cannot answer mentions. Set the GitHub App's webhook secret to the one kritik holds.
          </p>
        {:else}
          <p class="notice" role="note">
            No webhook has reached <span class="mono">{inst.name}</span>, so kritik only polls it for new pull requests and
            cannot answer mentions. Point the GitHub App's webhook at <span class="mono">{hookURL(inst.hookPath)}</span>.
          </p>
        {/if}
      </section>
    {/if}

    {#if open.data}
      {@const attention = stuck(open.data)}
      {#if attention.length}
        <section class="panel" aria-labelledby="an-attention">
          <header class="panel-head"><h2 id="an-attention">Needs attention</h2></header>
          <ul class="rows">
            {#each attention as a (a.outcome)}
              <li class="row">
                <a class="row-link" href={href({ name: 'pulls', slug, filter: { outcome: a.outcome } })}>
                  <Pill tone={reviewTone[a.outcome]} label={a.outcome} />
                  <span class="row-text">{a.text}</span>
                </a>
              </li>
            {/each}
          </ul>
        </section>
      {/if}
    {/if}

    <!-- The period scopes everything below it; what needs attention is now. -->
    <div class="toolbar">
      <Segmented label="Period" options={PERIODS} value={days} onchange={(d) => (days = d)} />
    </div>
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        {@const c = d.current}
        {@const p = d.previous}
        <div class="analytics" class:stale={res.loading}>
          <section class="stats stats-4" aria-label="Totals">
            <StatTile
              label="Pull requests reviewed"
              value={wholeNumber(c.pullRequests)}
              now={c.pullRequests}
              before={p.pullRequests}
              define="Pull requests with a completed review in the period"
            />
            <StatTile
              label="Reviews"
              value={wholeNumber(c.reviews)}
              sub={c.failed ? `${c.failed} failed` : ''}
              now={c.reviews}
              before={p.reviews}
              define="Completed reviews in the period"
            />
            <StatTile
              label="Findings"
              value={wholeNumber(found(c.findings))}
              sub={`${c.findings.blocking} blocking`}
              now={found(c.findings)}
              before={found(p.findings)}
              define="Findings first reported in the period, each counted once per pull request"
            />
            <StatTile
              label="Addressed"
              value={rate(c.addressed, found(c.findings)) === null ? '—' : `${Math.round(rate(c.addressed, found(c.findings))!)}%`}
              sub={`${c.addressed} of ${found(c.findings)}`}
              now={rate(c.addressed, found(c.findings))}
              before={rate(p.addressed, found(p.findings))}
              good="up"
              points
              define="Of those findings, the share a later review of the pull request, at a newer head, no longer reported"
            />
            <StatTile
              label="Median review"
              value={c.medianReviewMs === null ? '—' : duration(c.medianReviewMs)}
              now={c.medianReviewMs}
              before={p.medianReviewMs}
              good="down"
              define="How long a completed review took, from start to finish"
            />
            <StatTile
              label="Time to merge"
              value={c.medianMergeMs === null ? '—' : duration(c.medianMergeMs)}
              now={c.medianMergeMs}
              before={p.medianMergeMs}
              good="down"
              define="The median time from opening to merging, of the pull requests kritik knows that merged in the period"
            />
            <StatTile
              label="Reactions"
              value={`${wholeNumber(c.reactionsUp)} up`}
              sub={`${wholeNumber(c.reactionsDown)} down`}
              now={c.reactionsUp - c.reactionsDown}
              before={p.reactionsUp - p.reactionsDown}
              good="up"
              define="The thumbs up and down on the inline comments of the findings reported in the period"
            />
            <StatTile label="Spend" value={usd(c.costUsd)} now={c.costUsd} before={p.costUsd} good="down" define="Model spend in the period" />
          </section>

          <div class="grid-2">
            <section class="panel" aria-labelledby="an-reviews">
              <header class="panel-head">
                <h2 id="an-reviews">Reviews</h2>
                <Segmented label="Reviews view" options={VIEWS} value={reviewsView} onchange={(v) => (reviewsView = v)} />
              </header>
              <ColumnChart
                label="Completed reviews per {group}"
                series={[{ label: 'Reviews', color: 'var(--chart-ink)' }]}
                rows={rows(d.series, (x) => [x.reviews])}
                format={wholeNumber}
                view={reviewsView}
              />
            </section>
            <section class="panel" aria-labelledby="an-findings">
              <header class="panel-head">
                <h2 id="an-findings">Findings by severity</h2>
                <Segmented label="Findings view" options={VIEWS} value={findingsView} onchange={(v) => (findingsView = v)} />
              </header>
              <ColumnChart
                label="Findings first reported per {group}, by severity"
                series={SEV_SERIES}
                rows={rows(d.series, (x) => [x.findings.blocking, x.findings.important, x.findings.nit])}
                format={wholeNumber}
                view={findingsView}
              />
              <p class="panel-foot"><a href={href({ name: 'findings', slug })}>See every finding</a></p>
            </section>
          </div>

          <section class="panel" aria-labelledby="an-repos">
            <header class="panel-head"><h2 id="an-repos">Most reviewed repositories</h2></header>
            {#if d.repositories.length === 0}
              <p class="state-msg">No reviews in this period.</p>
            {:else}
              <div class="table-wrap">
                <table class="data">
                  <thead>
                    <tr>
                      <th scope="col">Repository</th>
                      <th scope="col" class="num">Reviews</th>
                      <th scope="col">Findings</th>
                      <th scope="col" class="num">Addressed</th>
                    </tr>
                  </thead>
                  <tbody>
                    {#each d.repositories as r (r.repository)}
                      {@const n = found(r.findings)}
                      <tr>
                        <td class="mono"><a href={href(repoRoute(slug, r.repository))}>{r.repository}</a></td>
                        <td class="num">{wholeNumber(r.reviews)}</td>
                        <td><SeverityCounts counts={r.findings} /></td>
                        <td class="num">{n ? `${Math.round((r.addressed / n) * 100)}%` : '—'}</td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            {/if}
          </section>
        </div>
      {/snippet}
    </StateView>
  </div>
</main>
