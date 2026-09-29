<script lang="ts">
  import { accountApi } from '../links';
  import { getJSON } from '../api.svelte';
  import { Resource, live } from '../resource.svelte';
  import { daysAgo, tokens, usd, wholeNumber } from '../format';
  import type { AccountSummary, UsageGroup, UsagePoint, UsageSeries } from '../types';
  import StateView from '../components/StateView.svelte';
  import ColumnChart from '../components/ColumnChart.svelte';
  import Segmented from '../components/Segmented.svelte';
  import StatTile from '../components/StatTile.svelte';
  import Meter from '../components/Meter.svelte';
  import SectionTabs from '../components/SectionTabs.svelte';

  let { slug }: { slug: string } = $props();
  const PERIODS = [
    { value: '7', label: '7 days' },
    { value: '30', label: '30 days' },
    { value: '90', label: '90 days' },
  ] as const;
  const GROUPS: readonly { value: UsageGroup; label: string }[] = [
    { value: 'day', label: 'Day' },
    { value: 'model', label: 'Model' },
    { value: 'repo', label: 'Repository' },
    { value: 'role', label: 'Role' },
  ];
  const METRICS = [
    { value: 'cost', label: 'Cost' },
    { value: 'tokens', label: 'Tokens' },
  ] as const;

  let days = $state<'7' | '30' | '90'>('30');
  let group = $state<UsageGroup>('day');
  let metric = $state<'cost' | 'tokens'>('cost');

  const total = (p: UsagePoint) => p.inputTokens + p.cacheReadTokens + p.cacheWriteTokens + p.outputTokens;

  const res = new Resource(() => {
    const p = new URLSearchParams({ group, from: daysAgo(Number(days), Date.now()) });
    return getJSON<UsageSeries>(`${accountApi(slug)}/usage?${p}`);
  });
  // The month so far against the account's caps.
  const summary = new Resource(() => getJSON<AccountSummary[]>('/api/v1/accounts'));
  const month = $derived(summary.data?.find((t) => t.slug === slug)?.usage);
  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void summary.load();
  });
  $effect(() => live((e) => e.account === slug && e.kind === 'model_call', () => void res.load(), 2000));

  function sum(rows: UsagePoint[]): UsagePoint {
    const z: UsagePoint = { key: 'Total', inputTokens: 0, cacheReadTokens: 0, cacheWriteTokens: 0, outputTokens: 0, costUsd: 0, calls: 0 };
    for (const r of rows) {
      z.inputTokens += r.inputTokens;
      z.cacheReadTokens += r.cacheReadTokens;
      z.cacheWriteTokens += r.cacheWriteTokens;
      z.outputTokens += r.outputTokens;
      z.costUsd += r.costUsd;
      z.calls += r.calls;
    }
    return z;
  }

  function columns(rows: UsagePoint[]) {
    return rows.map((r) => ({
      key: r.key,
      label: group === 'day' ? r.key.slice(5) : (r.key.split('/').pop() ?? r.key),
      title: r.key || '(none)',
      values: [metric === 'cost' ? r.costUsd : total(r)],
    }));
  }
</script>

<svelte:head><title>Spend · {slug} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <SectionTabs section="analytics" {slug} current="usage" />
    {#if month}
      <section class="stats" aria-label="This month">
        <StatTile label="Spend this month" value={usd(month.costUsd)} />
        <div class="stat">
          <span class="stat-label">Tokens this month</span>
          <span class="stat-value" title={wholeNumber(month.tokens)}>{tokens(month.tokens)}</span>
          <span class="stat-sub">{month.tokensPerMonth ? `of ${tokens(month.tokensPerMonth)} cap` : 'no monthly cap'}</span>
          <Meter value={month.tokens} max={month.tokensPerMonth} label="Monthly tokens used" />
        </div>
        <div class="stat">
          <span class="stat-label">Reviews today</span>
          <span class="stat-value">{wholeNumber(month.reviewsToday)}</span>
          <span class="stat-sub">{month.reviewsPerDay ? `of ${month.reviewsPerDay} a day` : 'no daily cap'}</span>
          <Meter value={month.reviewsToday} max={month.reviewsPerDay} label="Reviews today" />
        </div>
      </section>
    {/if}
    <div class="toolbar">
      <Segmented label="Period" options={PERIODS} value={days} onchange={(d) => (days = d)} />
      <Segmented label="Group by" options={GROUPS} value={group} onchange={(g) => (group = g)} />
      <Segmented label="Chart metric" options={METRICS} value={metric} onchange={(m) => (metric = m)} />
    </div>
    <StateView {res} retry={() => res.load()} isEmpty={(s) => s.rows.length === 0} empty="No model usage in this range.">
      {#snippet children(s)}
        {@const t = sum(s.rows)}
        <section class="panel" aria-labelledby="usage-chart">
          <header class="panel-head"><h2 id="usage-chart">{metric === 'cost' ? 'Cost' : 'Tokens'} by {s.group}</h2></header>
          <ColumnChart
            label="{metric === 'cost' ? 'Cost' : 'Tokens'} by {s.group}, last {days} days"
            series={[{ label: metric === 'cost' ? 'Cost' : 'Tokens', color: 'var(--chart-ink)' }]}
            rows={columns(s.rows)}
            format={metric === 'cost' ? usd : tokens}
          />
        </section>
        <div class="table-wrap table-card">
          <table class="data">
            <thead>
              <tr>
                <th scope="col">{s.group}</th><th scope="col" class="num">Calls</th><th scope="col" class="num">Input</th>
                <th scope="col" class="num">Cache read</th><th scope="col" class="num">Cache write</th><th scope="col" class="num">Output</th>
                <th scope="col" class="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {#each s.rows as r, i (i)}
                <tr>
                  <td class="mono small">{r.key}</td>
                  <td class="num">{wholeNumber(r.calls)}</td>
                  <td class="num" title={wholeNumber(r.inputTokens)}>{tokens(r.inputTokens)}</td>
                  <td class="num" title={wholeNumber(r.cacheReadTokens)}>{tokens(r.cacheReadTokens)}</td>
                  <td class="num" title={wholeNumber(r.cacheWriteTokens)}>{tokens(r.cacheWriteTokens)}</td>
                  <td class="num" title={wholeNumber(r.outputTokens)}>{tokens(r.outputTokens)}</td>
                  <td class="num">{usd(r.costUsd)}</td>
                </tr>
              {/each}
            </tbody>
            <tfoot>
              <tr>
                <th scope="row">Total</th>
                <td class="num">{wholeNumber(t.calls)}</td>
                <td class="num">{tokens(t.inputTokens)}</td>
                <td class="num">{tokens(t.cacheReadTokens)}</td>
                <td class="num">{tokens(t.cacheWriteTokens)}</td>
                <td class="num">{tokens(t.outputTokens)}</td>
                <td class="num">{usd(t.costUsd)}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      {/snippet}
    </StateView>
  </div>
</main>
