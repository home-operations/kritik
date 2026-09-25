<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { Resource, live } from '../resource.svelte';
  import { daysAgo, tokens, usd, wholeNumber } from '../format';
  import type { UsageGroup, UsagePoint, UsageSeries } from '../types';
  import StateView from '../components/StateView.svelte';
  import BarChart from '../components/BarChart.svelte';

  let { slug }: { slug: string } = $props();
  const PRESETS = [7, 30, 90] as const;
  const GROUPS: readonly UsageGroup[] = ['day', 'model', 'repo', 'role'];

  let days = $state<(typeof PRESETS)[number]>(30);
  let group = $state<UsageGroup>('day');
  let metric = $state<'cost' | 'tokens'>('cost');

  const total = (p: UsagePoint) => p.inputTokens + p.cacheReadTokens + p.cacheWriteTokens + p.outputTokens;

  const res = new Resource(() => {
    const p = new URLSearchParams({ group, from: daysAgo(days, Date.now()) });
    return getJSON<UsageSeries>(`/api/v1/tenants/${encodeURIComponent(slug)}/usage?${p}`);
  });
  $effect(() => {
    void res.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind === 'model_call', () => void res.load(), 2000));

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

  function bars(rows: UsagePoint[]) {
    return rows.map((r) => ({
      label: group === 'day' ? r.key.slice(5) : r.key.split('/').pop() ?? r.key,
      value: metric === 'cost' ? r.costUsd : total(r),
      title: `${r.key}: ${usd(r.costUsd)}, ${wholeNumber(total(r))} tokens, ${r.calls} calls`,
    }));
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Usage</h1></header>
    <div class="toolbar">
      <div class="view-toggle" role="group" aria-label="Date range">
        {#each PRESETS as p (p)}
          <button class:active={days === p} aria-pressed={days === p} onclick={() => (days = p)}>{p}d</button>
        {/each}
      </div>
      <div class="view-toggle" role="group" aria-label="Group by">
        {#each GROUPS as g (g)}
          <button class:active={group === g} aria-pressed={group === g} onclick={() => (group = g)}>{g}</button>
        {/each}
      </div>
      <div class="view-toggle" role="group" aria-label="Chart metric">
        <button class:active={metric === 'cost'} aria-pressed={metric === 'cost'} onclick={() => (metric = 'cost')}>cost</button>
        <button class:active={metric === 'tokens'} aria-pressed={metric === 'tokens'} onclick={() => (metric = 'tokens')}>tokens</button>
      </div>
    </div>
    <StateView {res} retry={() => res.load()} isEmpty={(s) => s.rows.length === 0} empty="No model usage in this range.">
      {#snippet children(s)}
        {@const t = sum(s.rows)}
        <section class="panel" aria-label="Usage chart">
          <BarChart
            bars={bars(s.rows)}
            label="{metric === 'cost' ? 'Cost' : 'Tokens'} by {s.group}, last {days} days"
            format={metric === 'cost' ? usd : tokens}
          />
        </section>
        <div class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th scope="col">{s.group}</th><th scope="col" class="num">Calls</th><th scope="col" class="num">Input</th>
                <th scope="col" class="num">Cache read</th><th scope="col" class="num">Cache write</th><th scope="col" class="num">Output</th>
                <th scope="col" class="num">Cost</th>
              </tr>
            </thead>
            <tbody>
              {#each s.rows as r (r.key)}
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
