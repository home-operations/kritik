<script lang="ts">
  // The instance's configuration for an admin, read-only (ADR-0019 §2.2):
  // what setup still lacks, the accounts its connections serve, each
  // setting with where it comes from, the stored instance configuration,
  // the connections and their installations, and the admin audit log.
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { tokens, usd } from '../format';
  import type { InstanceConfig, InstanceSetting, AdminAccount } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import AuditTable from '../components/AuditTable.svelte';
  import Checklist from './admin/Checklist.svelte';
  import ConnectionsSection from './admin/ConnectionsSection.svelte';
  import SpecView from './admin/SpecView.svelte';

  const res = new Resource(() => getJSON<AdminAccount[]>('/api/v1/admin/accounts'));
  const instance = new Resource(() => getJSON<InstanceSetting[]>('/api/v1/admin/instance'));
  const config = new Resource(() => getJSON<InstanceConfig>('/api/v1/config'));
  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void instance.load();
  });
  $effect(() => {
    void config.load();
  });
  const sourceLabel: Record<string, string> = { env: 'environment', file: 'config file', default: 'default', dashboard: 'dashboard' };
</script>

<svelte:head><title>Configuration · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Configuration</h1>
      <p class="muted">What the instance runs. It is read from the configuration file and changed there; the dashboard does not edit it.</p>
    </header>
    {#if res.data}<Checklist accounts={res.data} />{/if}
    <section class="panel" aria-labelledby="op-accounts">
      <header class="panel-head"><h2 id="op-accounts">Accounts</h2></header>
      <p class="muted small panel-body">Every account a connection serves, and entries of the configuration no connection serves.</p>
      <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="No accounts yet: declare a connection in the configuration file.">
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
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/snippet}
      </StateView>
    </section>

    <section class="panel" aria-labelledby="op-instance">
      <header class="panel-head"><h2 id="op-instance">Instance settings</h2></header>
      <p class="muted small panel-body">
        The environment is this web process's; sign-in, the file's connections and the instance defaults it sets are the
        configuration file's, which the stored instance configuration below may override.
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

    <section class="panel" aria-labelledby="op-config">
      <header class="panel-head">
        <h2 id="op-config">Instance configuration</h2>
        {#if config.data}<span class="small muted">revision {config.data.revision}</span>{/if}
      </header>
      <StateView res={config} retry={() => config.load()}>
        {#snippet children(cfg)}<SpecView spec={cfg.spec} />{/snippet}
      </StateView>
    </section>

    <ConnectionsSection />

    <section class="panel" aria-labelledby="op-audit">
      <header class="panel-head"><h2 id="op-audit">Admin audit log</h2></header>
      <AuditTable path="/api/v1/admin/audit" showAccount />
    </section>
  </div>
</main>
