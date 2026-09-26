<script lang="ts">
  // The landing page: totals across every tenant the viewer can see, then
  // one row per tenant with its own numbers, each linking to the tenant.
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { tokens, usd, wholeNumber } from '../format';
  import type { TenantSummary } from '../types';
  import StateView from '../components/StateView.svelte';
  import Meter from '../components/Meter.svelte';

  const res = new Resource(() => getJSON<TenantSummary[]>('/api/v1/tenants'));

  $effect(() => {
    void res.load();
  });
  $effect(() => live((e) => e.kind !== 'model_call', () => void res.load()));

  function totals(list: TenantSummary[]) {
    const sum = (f: (t: TenantSummary) => number) => list.reduce((n, t) => n + f(t), 0);
    return {
      installations: sum((t) => t.installations),
      repositories: sum((t) => t.repositories),
      reviews7d: sum((t) => t.reviews7d),
      reviewsToday: sum((t) => t.usage.reviewsToday),
      tokens: sum((t) => t.usage.tokens),
      costUsd: sum((t) => t.usage.costUsd),
    };
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>All tenants</h1></header>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="You are not a member of any tenant yet.">
      {#snippet children(list)}
        {@const all = totals(list)}
        <section class="tiles" aria-label="Across all tenants">
          <div class="tile">
            <span class="tile-label">Tenants</span>
            <span class="tile-value">{wholeNumber(list.length)}</span>
            <span class="small muted">{wholeNumber(all.installations)} installations</span>
          </div>
          <div class="tile">
            <span class="tile-label">Repositories</span>
            <span class="tile-value">{wholeNumber(all.repositories)}</span>
          </div>
          <div class="tile">
            <span class="tile-label">Reviews, last 7 days</span>
            <span class="tile-value">{wholeNumber(all.reviews7d)}</span>
            <span class="small muted">{wholeNumber(all.reviewsToday)} today</span>
          </div>
          <div class="tile">
            <span class="tile-label">Spend this month</span>
            <span class="tile-value">{usd(all.costUsd)}</span>
          </div>
          <div class="tile">
            <span class="tile-label">Tokens this month</span>
            <span class="tile-value" title={wholeNumber(all.tokens)}>{tokens(all.tokens)}</span>
          </div>
        </section>

        <section class="panel" aria-labelledby="tenant-breakdown">
          <header class="panel-head"><h2 id="tenant-breakdown">By tenant</h2></header>
          <div class="table-wrap">
            <table class="data tenant-breakdown">
              <thead>
                <tr>
                  <th scope="col">Tenant</th>
                  <th scope="col">Role</th>
                  <th scope="col" class="num">Installations</th>
                  <th scope="col" class="num">Repositories</th>
                  <th scope="col" class="num">Reviews 7d</th>
                  <th scope="col" class="num">Spend</th>
                  <th scope="col">Tokens this month</th>
                </tr>
              </thead>
              <tbody>
                {#each list as t (t.slug)}
                  <tr>
                    <td class="mono"><a href={href({ name: 'tenant', slug: t.slug })}>{t.slug}</a></td>
                    <td>{t.role} <span class="muted small">({t.managedBy})</span></td>
                    <td class="num">{wholeNumber(t.installations)}</td>
                    <td class="num">{wholeNumber(t.repositories)}</td>
                    <td class="num">{wholeNumber(t.reviews7d)}</td>
                    <td class="num">{usd(t.usage.costUsd)}</td>
                    <td>
                      <span class="small">
                        {tokens(t.usage.tokens)}{t.usage.tokensPerMonth ? ` of ${tokens(t.usage.tokensPerMonth)}` : ''}
                      </span>
                      {#if t.usage.tokensPerMonth}
                        <Meter value={t.usage.tokens} max={t.usage.tokensPerMonth} label={`Monthly tokens used by ${t.slug}`} />
                      {/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </section>
      {/snippet}
    </StateView>
  </div>
</main>
