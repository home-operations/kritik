<script lang="ts">
  import { ApiError, getJSON, sendJSON } from '../api.svelte';
  import { href, setLeaveGuard } from '../router.svelte';
  import { Resource } from '../resource.svelte';
  import { tokens, usd } from '../format';
  import { describe, errorPath, isCode } from '../manage';
  import { MANAGEMENT_OFF, management } from '../session.svelte';
  import { toast } from '../toast.svelte';
  import type { CreateTenantRequest, OperatorTenant, SlugTakenDetails, TenantWriteResult } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Dialog from '../components/Dialog.svelte';
  import AuditTable from '../components/AuditTable.svelte';
  import ConfigEditor from './admin/ConfigEditor.svelte';
  import GeneratedSecrets from './admin/GeneratedSecrets.svelte';

  const res = new Resource(() => getJSON<OperatorTenant[]>('/api/v1/operator/tenants'));
  $effect(() => {
    void res.load();
  });

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
    if (dirty && !window.confirm('Discard the new tenant you have started?')) return;
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
    const body: CreateTenantRequest = { slug: typeof spec.slug === 'string' ? spec.slug : '', spec };
    if (adopt) body.adopt = true;
    try {
      const r = await sendJSON<TenantWriteResult>('POST', '/api/v1/tenants', body);
      toast(`Created tenant ${r.slug}`);
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

  let target = $state<OperatorTenant | undefined>(undefined);
  let deleteOpen = $state(false);
  let typed = $state('');
  let deleting = $state(false);
  let deleteError = $state('');

  function askDelete(t: OperatorTenant): void {
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
      await sendJSON('DELETE', `/api/v1/tenants/${encodeURIComponent(t.slug)}?revision=${t.revision}`);
      toast(`Deleted tenant ${t.slug}`);
      deleteOpen = false;
      void res.load();
    } catch (err) {
      deleteError = describe(err);
      if (isCode(err, 'revision_conflict')) {
        await res.load();
        const fresh = res.data?.find((x) => x.slug === t.slug);
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
      <h1>Operator console</h1>
      <p class="muted">Every tenant in the running configuration, plus dashboard tenants that are stored but not live.</p>
    </header>
    {#if !management()}
      <p class="notice" role="note">{MANAGEMENT_OFF} Tenants cannot be created or deleted here.</p>
    {:else if creating}
      <section class="panel" aria-labelledby="op-create">
        <header class="panel-head">
          <h2 id="op-create">New dashboard tenant</h2>
          <button class="btn btn-small" onclick={cancelCreate}>Cancel</button>
        </header>
        <div class="panel-body">
          <ConfigEditor initial={{}} creating operator {saving} {errMessage} {errPath} {errSeq} bind:dirty submitLabel="Create tenant" onsave={create} />
          {#if offerAdopt}
            <div class="notice" role="note">
              <label>
                <input type="checkbox" bind:checked={adopt} />
                Adopt this slug
              </label>
              <p class="muted">
                A tenant used this slug before. Adopting it removes that tenant's members and invites, but its reviews, findings
                and transcripts are kept and become visible to the new tenant's members.
              </p>
            </div>
          {/if}
        </div>
      </section>
    {:else}
      <div class="page-actions">
        <button class="btn btn-primary" onclick={() => (creating = true)}>New tenant</button>
      </div>
    {/if}
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
                <th scope="col"><span class="sr-only">Actions</span></th>
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
                  <td>
                    {#if t.managedBy === 'dashboard'}
                      <a class="btn btn-small" href={href({ name: 'admin', slug: t.slug, section: 'config' })}>Edit</a>
                      {#if management()}
                        <button class="btn btn-small btn-danger" onclick={() => askDelete(t)} aria-label={`Delete tenant ${t.slug}`}>Delete</button>
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

    <section class="panel" aria-labelledby="op-audit">
      <header class="panel-head"><h2 id="op-audit">Operator audit log</h2></header>
      <AuditTable path="/api/v1/operator/audit" showTenant />
    </section>
  </div>
</main>

<Dialog bind:open={deleteOpen} title={`Delete tenant ${target?.slug ?? ''}?`}>
  <form class="form" id="delete-tenant" onsubmit={remove}>
    <p>
      This deletes the dashboard tenant <span class="mono">{target?.slug}</span> and all access granted to it.
    </p>
    <label class="field">
      <span>Type the slug to confirm</span>
      <input class="mono" autocomplete="off" bind:value={typed} />
    </label>
    <div aria-live="assertive">{#if deleteError}<p class="form-alert" role="alert">{deleteError}</p>{/if}</div>
  </form>
  {#snippet footer()}
    <button class="btn" onclick={() => (deleteOpen = false)}>Keep</button>
    <button class="btn btn-primary btn-danger" type="submit" form="delete-tenant" disabled={deleting || typed !== target?.slug}>
      {deleting ? 'Deleting…' : 'Delete tenant'}
    </button>
  {/snippet}
</Dialog>

<GeneratedSecrets bind:generated />
