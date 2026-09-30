<script lang="ts">
  // The running connections, and the accounts one's GitHub App is
  // installed on (ADR-0014 §2.3): kritik serves only the accounts a
  // connection lists, and an installation on any other can be removed.
  import { getJSON, sendJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import { describe } from '../../manage';
  import { toast } from '../../toast.svelte';
  import type { AppInstallation, Connection } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import Pill from '../../components/Pill.svelte';
  import Time from '../../components/Time.svelte';
  import Dialog from '../../components/Dialog.svelte';

  const conns = new Resource(() => getJSON<Connection[]>('/api/v1/admin/connections'));
  $effect(() => {
    void conns.load();
  });

  let selected = $state('');
  const installationsPath = $derived(`/api/v1/admin/connections/${encodeURIComponent(selected)}/installations`);
  const installs = new Resource(() => getJSON<AppInstallation[]>(installationsPath));

  function show(name: string): void {
    selected = name;
    void installs.load();
  }

  let removing = $state<AppInstallation | undefined>(undefined);
  let confirmOpen = $state(false);
  let busy = $state(false);

  function ask(inst: AppInstallation): void {
    removing = inst;
    confirmOpen = true;
  }

  async function uninstall(): Promise<void> {
    const inst = removing;
    if (!inst) return;
    busy = true;
    try {
      await sendJSON('DELETE', `${installationsPath}/${inst.id}`);
      toast(`Uninstalled from ${inst.account}`);
    } catch (err) {
      toast(`Uninstall failed: ${describe(err)}`, 'danger');
    } finally {
      busy = false;
      confirmOpen = false;
      void installs.load();
    }
  }
</script>

<section class="panel" aria-labelledby="op-connections">
  <header class="panel-head"><h2 id="op-connections">GitHub Apps</h2></header>
  <StateView res={conns} retry={() => conns.load()} isEmpty={(d) => d.length === 0} empty="No GitHub Apps yet.">
    {#snippet children(list)}
      <div class="table-wrap">
        <table class="data">
          <thead>
            <tr>
              <th scope="col">App</th>
              <th scope="col">From</th>
              <th scope="col">Accounts</th>
              <th scope="col">Webhooks</th>
              <th scope="col"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {#each list as c (c.name)}
              <tr>
                <td class="mono">{c.name}</td>
                <td>{c.managedBy === 'file' ? 'config file' : 'dashboard'}</td>
                <td class="mono small">{c.accounts.join(', ')}</td>
                <td>
                  {#if c.lastWebhookAt}<Pill tone="ok" label="receiving" /> <span class="small muted"><Time iso={c.lastWebhookAt} /></span>
                  {:else}<Pill tone="warn" label="none yet" />{/if}
                </td>
                <td>
                  <button class="btn btn-small" aria-pressed={selected === c.name} onclick={() => show(c.name)}>Installations</button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/snippet}
  </StateView>

  {#if selected}
    <h3 class="small">GitHub installations of <span class="mono">{selected}</span></h3>
    <StateView res={installs} retry={() => installs.load()} isEmpty={(d) => d.length === 0} empty="The App is not installed anywhere yet.">
      {#snippet children(list)}
        <div class="table-wrap">
          <table class="data" aria-label={`Installations of ${selected}`}>
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Repositories</th>
                <th scope="col">State</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {#each list as inst (inst.id)}
                <tr>
                  <td>
                    {#if inst.url}<a class="mono" href={inst.url} target="_blank" rel="noopener noreferrer">{inst.account}</a>
                    {:else}<span class="mono">{inst.account}</span>{/if}
                    <span class="small muted">{inst.accountType === 'Organization' ? 'organization' : 'user'}</span>
                  </td>
                  <td>{inst.allRepositories ? 'all' : 'selected'}</td>
                  <td>
                    {#if inst.served}<Pill tone="ok" label="served" />{:else}<Pill tone="warn" label="not served" />{/if}
                    {#if inst.suspended}<Pill tone="muted" label="suspended" />{/if}
                  </td>
                  <td>
                    {#if !inst.served}
                      <button class="btn btn-small btn-danger" onclick={() => ask(inst)}>Uninstall from {inst.account}</button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/snippet}
    </StateView>
  {/if}
</section>

<Dialog bind:open={confirmOpen} title="Uninstall the App?" fallback="#op-connections">
  <p>
    GitHub removes <span class="mono">{selected}</span>'s App from <span class="mono">{removing?.account}</span>. kritik does
    not serve that account, so nothing it reviews changes.
  </p>
  {#snippet footer()}
    <button class="btn" onclick={() => (confirmOpen = false)}>Keep it</button>
    <button class="btn btn-primary btn-danger" onclick={uninstall} disabled={busy}>{busy ? 'Working…' : 'Uninstall'}</button>
  {/snippet}
</Dialog>
