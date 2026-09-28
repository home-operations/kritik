<script lang="ts">
  import { ApiError, getJSON, sendJSON } from '../api.svelte';
  import { href, setLeaveGuard } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { tokens, usd } from '../format';
  import { describe, errorPath, isCode } from '../manage';
  import { MANAGEMENT_OFF, management } from '../session.svelte';
  import { toast } from '../toast.svelte';
  import type { CreateAccountRequest, InstanceSetting, OperatorAccount, SlugTakenDetails, AccountWriteResult } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Dialog from '../components/Dialog.svelte';
  import AuditTable from '../components/AuditTable.svelte';
  import ConfigEditor from './admin/ConfigEditor.svelte';
  import GeneratedSecrets from './admin/GeneratedSecrets.svelte';

  const res = new Resource(() => getJSON<OperatorAccount[]>('/api/v1/operator/accounts'));
  const instance = new Resource(() => getJSON<InstanceSetting[]>('/api/v1/operator/instance'));
  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void instance.load();
  });
  const sourceLabel: Record<string, string> = { env: 'environment', file: 'config file', default: 'default' };

  let creating = $state(false);
  let saving = $state(false);
  let dirty = $state(false);
  let errMessage = $state('');
  let errPath = $state('');
  let errSeq = $state(0);
  let generated = $state<Record<string, string> | undefined>(undefined);
  // Offered once a create is refused because the slug was used before.
  let offerAdopt = $state(false);
  let adopt = $state(false);

  $effect(() => {
    setLeaveGuard(() => (creating && dirty) || generated !== undefined);
    return () => setLeaveGuard(undefined);
  });

  function cancelCreate(): void {
    if (dirty && !window.confirm('Discard the new account you have started?')) return;
    closeCreate();
  }

  function closeCreate(): void {
    creating = false;
    dirty = false;
    errMessage = '';
    errPath = '';
    offerAdopt = false;
    adopt = false;
  }

  async function create(spec: Record<string, unknown>): Promise<void> {
    saving = true;
    errMessage = '';
    errPath = '';
    const body: CreateAccountRequest = { slug: typeof spec.slug === 'string' ? spec.slug : '', spec };
    if (adopt) body.adopt = true;
    try {
      const r = await sendJSON<AccountWriteResult>('POST', '/api/v1/accounts', body);
      toast(`Created account ${r.slug}`);
      if (r.generated && Object.keys(r.generated).length) generated = r.generated;
      closeCreate();
      void res.load();
    } catch (err) {
      errMessage = describe(err);
      errPath = errorPath(err);
      if (err instanceof ApiError && err.code === 'slug_taken' && (err.details as SlugTakenDetails | undefined)?.adoptable) offerAdopt = true;
      errSeq++;
    } finally {
      saving = false;
    }
  }

  let target = $state<OperatorAccount | undefined>(undefined);
  let deleteOpen = $state(false);
  let typed = $state('');
  let deleting = $state(false);
  let deleteError = $state('');

  function askDelete(t: OperatorAccount): void {
    target = t;
    typed = '';
    deleteError = '';
    deleteOpen = true;
  }

  async function remove(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    const t = target;
    if (!t || typed !== t.slug) return;
    deleting = true;
    deleteError = '';
    try {
      await sendJSON('DELETE', `/api/v1/accounts/${encodeURIComponent(t.slug)}?revision=${t.revision}`);
      toast(`Deleted account ${t.slug}`);
      deleteOpen = false;
      void res.load();
    } catch (err) {
      deleteError = describe(err);
      if (isCode(err, 'revision_conflict')) {
        await res.load();
        const fresh = res.data?.find((x) => x.slug === t.slug && x.managedBy === t.managedBy);
        if (fresh) target = fresh;
        deleteError += ' The latest revision is loaded; confirm again to delete it.';
      }
    } finally {
      deleting = false;
    }
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Admin console</h1>
      <p class="muted">
        Every account in the running configuration, plus dashboard accounts that are stored but not live and file accounts a
        conflict leaves out.
      </p>
    </header>
    {#if !management()}
      <p class="notice" role="note">{MANAGEMENT_OFF} Accounts cannot be created or deleted here.</p>
    {:else if creating}
      <section class="panel" aria-labelledby="op-create">
        <header class="panel-head">
          <h2 id="op-create">New dashboard account</h2>
          <button class="btn btn-small" onclick={cancelCreate}>Cancel</button>
        </header>
        <div class="panel-body">
          <ConfigEditor initial={{}} creating editable={() => true} {saving} {errMessage} {errPath} {errSeq} bind:dirty submitLabel="Create account" onsave={create} />
          {#if offerAdopt}
            <div class="notice" role="note">
              <label>
                <input type="checkbox" bind:checked={adopt} />
                Adopt this slug
              </label>
              <p class="muted">
                An account used this slug before. Adopting it keeps that account's reviews, findings and transcripts, which become
                the new account's.
              </p>
            </div>
          {/if}
        </div>
      </section>
    {:else}
      <div class="page-actions">
        <button class="btn btn-primary" onclick={() => (creating = true)}>New account</button>
      </div>
    {/if}
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="No accounts configured.">
      {#snippet children(list)}
        <div class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Managed by</th>
                <th scope="col">State</th>
                <th scope="col" class="num">Revision</th>
                <th scope="col" class="num">Connections</th>
                <th scope="col" class="num">Repos</th>
                <th scope="col" class="num">Reviews 7d</th>
                <th scope="col" class="num">Tokens (month)</th>
                <th scope="col" class="num">Spend (month)</th>
                <th scope="col"><span class="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {#each list as t (`${t.managedBy}:${t.slug}`)}
                <tr>
                  <td class="mono">
                    {#if t.live}<a href={href({ name: 'account', slug: t.slug })}>{t.slug}</a>{:else}{t.slug}{/if}
                  </td>
                  <td>{t.managedBy}</td>
                  <td>
                    {#if t.conflict}
                      <Pill tone="danger" label="conflict" />
                      <span class="small muted">{t.conflict}</span>
                    {:else}
                      <Pill
                        tone={t.live ? 'ok' : 'warn'}
                        label={t.live ? 'live' : 'not live'}
                        title={t.live ? undefined : 'Stored but not in the running configuration'}
                      />
                    {/if}
                  </td>
                  <td class="num">{t.revision || '—'}</td>
                  <td class="num">{t.connections}</td>
                  <td class="num">{t.repositories}</td>
                  <td class="num">{t.reviews7d}</td>
                  <td class="num">{tokens(t.usage.tokens)}</td>
                  <td class="num">{usd(t.usage.costUsd)}</td>
                  <td>
                    {#if t.managedBy === 'dashboard'}
                      <a class="btn btn-small" href={href({ name: 'admin', slug: t.slug, section: 'config' })}>Edit</a>
                      {#if management()}
                        <button class="btn btn-small btn-danger" onclick={() => askDelete(t)} aria-label={`Delete account ${t.slug}`}>Delete</button>
                      {/if}
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/snippet}
    </StateView>

    <section class="panel" aria-labelledby="op-instance">
      <header class="panel-head"><h2 id="op-instance">Instance settings</h2></header>
      <p class="muted small">
        Read-only: the environment is this web process's, and the rest is the configuration file's or kritik's defaults.
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

<Dialog bind:open={deleteOpen} title={`Delete account ${target?.slug ?? ''}?`}>
  <form class="form" id="delete-account" onsubmit={remove}>
    <p>
      This deletes the dashboard account <span class="mono">{target?.slug}</span> and all access granted to it.
    </p>
    <label class="field">
      <span>Type the slug to confirm</span>
      <input class="mono" autocomplete="off" bind:value={typed} />
    </label>
    <div aria-live="assertive">{#if deleteError}<p class="form-alert" role="alert">{deleteError}</p>{/if}</div>
  </form>
  {#snippet footer()}
    <button class="btn" onclick={() => (deleteOpen = false)}>Keep</button>
    <button class="btn btn-primary btn-danger" type="submit" form="delete-account" disabled={deleting || typed !== target?.slug}>
      {deleting ? 'Deleting…' : 'Delete account'}
    </button>
  {/snippet}
</Dialog>

<GeneratedSecrets bind:generated />
