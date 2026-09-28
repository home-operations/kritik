<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { tokens, usd } from '../format';
  import type { InstanceSetting, OperatorAccount } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import AuditTable from '../components/AuditTable.svelte';
  import InstanceSection from './admin/InstanceSection.svelte';
  import AppSetup from './admin/AppSetup.svelte';
  import ConnectionsSection from './admin/ConnectionsSection.svelte';
  import { management } from '../session.svelte';

  const res = new Resource(() => getJSON<OperatorAccount[]>('/api/v1/operator/accounts'));
  const instance = new Resource(() => getJSON<InstanceSetting[]>('/api/v1/operator/instance'));
  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void instance.load();
  });
  const sourceLabel: Record<string, string> = { env: 'environment', file: 'config file', default: 'default', dashboard: 'dashboard' };

  // A saved spec changes which accounts run and which connections the
  // settings list.
  function refresh(): void {
    void res.load();
    void instance.load();
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Admin console</h1>
      <p class="muted">Every account a connection serves, and entries of the instance configuration no connection serves.</p>
    </header>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="No accounts yet: add a connection below.">
      {#snippet children(list)}
        <div class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Connection</th>
                <th scope="col">State</th>
                <th scope="col" class="num">Repos</th>
                <th scope="col" class="num">Reviews 7d</th>
                <th scope="col" class="num">Tokens (month)</th>
                <th scope="col" class="num">Spend (month)</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {#each list as t (t.slug)}
                <tr>
                  <td class="mono">
                    {#if t.live}<a href={href({ name: 'account', slug: t.slug })}>{t.slug}</a>{:else}{t.slug}{/if}
                  </td>
                  <td class="mono small">{t.connection || '—'}</td>
                  <td>
                    {#if t.conflict}
                      <Pill tone="warn" label="not served" />
                      <span class="small muted">{t.conflict}</span>
                    {:else}
                      <Pill tone="ok" label="live" />
                    {/if}
                  </td>
                  <td class="num">{t.repositories}</td>
                  <td class="num">{t.reviews7d}</td>
                  <td class="num">{tokens(t.usage.tokens)}</td>
                  <td class="num">{usd(t.usage.costUsd)}</td>
                  <td>
                    {#if t.live}
                      <a class="btn btn-small" href={href({ name: 'admin', slug: t.slug, section: 'config' })}>Edit</a>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/snippet}
    </StateView>

    <InstanceSection onsaved={refresh} />

    <ConnectionsSection />

    {#if management()}<AppSetup />{/if}

    <section class="panel" aria-labelledby="op-instance">
      <header class="panel-head"><h2 id="op-instance">Instance settings</h2></header>
      <p class="muted small">
        Read-only: the environment is this web process's, and sign-in and the file's connections are the configuration file's.
      </p>
      <StateView res={instance} retry={() => instance.load()} isEmpty={(d) => d.length === 0} empty="No instance settings.">
        {#snippet children(rows)}
          <div class="table-wrap">
            <table class="data">
              <thead>
                <tr><th scope="col">Section</th><th scope="col">Setting</th><th scope="col">Value</th><th scope="col">Source</th></tr>
              </thead>
              <tbody>
                {#each rows as row (`${row.section}:${row.key}`)}
                  <tr>
                    <td>{row.section}</td>
                    <td class="mono">{row.key}</td>
                    <td class="mono">{row.value}</td>
                    <td>{sourceLabel[row.source] ?? row.source}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/snippet}
      </StateView>
    </section>

    <section class="panel" aria-labelledby="op-audit">
      <header class="panel-head"><h2 id="op-audit">Admin audit log</h2></header>
      <AuditTable path="/api/v1/operator/audit" showAccount />
    </section>
  </div>
</main>
