<script lang="ts">
  // The landing page: a member of exactly one tenant goes straight to it;
  // anyone else picks from cards.
  import { getJSON } from '../api.svelte';
  import { href, replace } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { tokens, usd } from '../format';
  import type { TenantSummary } from '../types';
  import StateView from '../components/StateView.svelte';
  import Meter from '../components/Meter.svelte';

  const res = new Resource(() => getJSON<TenantSummary[]>('/api/v1/tenants'));

  $effect(() => {
    void res.load();
  });

  $effect(() => {
    const only = res.data?.length === 1 ? res.data[0] : undefined;
    if (only) replace({ name: 'tenant', slug: only.slug });
  });
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Tenants</h1></header>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="You are not a member of any tenant yet.">
      {#snippet children(list)}
        <ul class="tenant-cards">
          {#each list as t (t.slug)}
            <li>
              <a class="tenant-card" href={href({ name: 'tenant', slug: t.slug })}>
                <span class="tenant-card-title mono">{t.slug}</span>
                <span class="muted small">{t.role} · {t.managedBy}</span>
                <span class="tenant-card-stats">
                  <span><strong>{t.repositories}</strong> repos</span>
                  <span><strong>{t.reviews7d}</strong> reviews 7d</span>
                  <span><strong>{usd(t.usage.costUsd)}</strong> this month</span>
                </span>
                <span class="small muted">{tokens(t.usage.tokens)} tokens</span>
                <Meter value={t.usage.tokens} max={t.usage.tokensPerMonth} label="Monthly tokens used" />
              </a>
            </li>
          {/each}
        </ul>
      {/snippet}
    </StateView>
  </div>
</main>
