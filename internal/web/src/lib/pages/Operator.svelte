<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { tokens, usd } from '../format';
  import type { OperatorTenant } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';

  const res = new Resource(() => getJSON<OperatorTenant[]>('/api/v1/operator/tenants'));
  $effect(() => {
    void res.load();
  });
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Operator console</h1>
      <p class="muted">Every tenant in the running configuration, plus dashboard tenants that are stored but not live.</p>
    </header>
    <!-- Tenant creation and other operator actions mount here. -->
    <div class="page-actions" data-slot="operator-actions"></div>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="No tenants configured.">
      {#snippet children(list)}
        <div class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th scope="col">Tenant</th>
                <th scope="col">Managed by</th>
                <th scope="col">State</th>
                <th scope="col" class="num">Revision</th>
                <th scope="col" class="num">Installations</th>
                <th scope="col" class="num">Repos</th>
                <th scope="col" class="num">Reviews 7d</th>
                <th scope="col" class="num">Tokens (month)</th>
                <th scope="col" class="num">Spend (month)</th>
              </tr>
            </thead>
            <tbody>
              {#each list as t (t.slug)}
                <tr>
                  <td class="mono">
                    {#if t.live}<a href={href({ name: 'tenant', slug: t.slug })}>{t.slug}</a>{:else}{t.slug}{/if}
                  </td>
                  <td>{t.managedBy}</td>
                  <td>
                    <Pill
                      tone={t.live ? 'ok' : 'warn'}
                      label={t.live ? 'live' : 'not live'}
                      title={t.live ? undefined : 'Stored but not in the running configuration'}
                    />
                  </td>
                  <td class="num">{t.revision || '—'}</td>
                  <td class="num">{t.installations}</td>
                  <td class="num">{t.repositories}</td>
                  <td class="num">{t.reviews7d}</td>
                  <td class="num">{tokens(t.usage.tokens)}</td>
                  <td class="num">{usd(t.usage.costUsd)}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/snippet}
    </StateView>
  </div>
</main>
