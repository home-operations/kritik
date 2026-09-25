<script lang="ts">
  import { getJSON, sendJSON } from '../../api.svelte';
  import { setLeaveGuard } from '../../router.svelte';
  import { Resource } from '../../resource.svelte';
  import { describe, errorPath, isCode } from '../../manage';
  import { MANAGEMENT_OFF, isOperator, management } from '../../session.svelte';
  import { toast } from '../../toast.svelte';
  import type { TenantConfig, TenantWriteResult } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import ConfigEditor from './ConfigEditor.svelte';
  import SpecView from './SpecView.svelte';
  import GeneratedSecrets from './GeneratedSecrets.svelte';

  let { slug }: { slug: string } = $props();
  const path = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/config`);
  const res = new Resource(() => getJSON<TenantConfig>(path));
  $effect(() => {
    void res.load();
  });

  let saving = $state(false);
  let dirty = $state(false);
  let errMessage = $state('');
  let errPath = $state('');
  let conflict = $state(false);
  let generated = $state<Record<string, string> | undefined>(undefined);
  // Bumped to remount the editor on a fresh draft.
  let epoch = $state(0);

  $effect(() => {
    setLeaveGuard(() => dirty);
    return () => setLeaveGuard(undefined);
  });

  function resetError(): void {
    errMessage = '';
    errPath = '';
    conflict = false;
  }

  async function reload(): Promise<void> {
    resetError();
    dirty = false;
    await res.load();
    epoch++;
  }

  async function save(cfg: TenantConfig, spec: Record<string, unknown>): Promise<void> {
    saving = true;
    resetError();
    try {
      const r = await sendJSON<TenantWriteResult>('PUT', path, { revision: cfg.revision, spec });
      toast(`Saved: revision ${r.revision}`);
      if (r.generated && Object.keys(r.generated).length) generated = r.generated;
      await reload();
    } catch (err) {
      errMessage = describe(err);
      errPath = errorPath(err);
      conflict = isCode(err, 'revision_conflict');
    } finally {
      saving = false;
    }
  }

  function readOnlyReason(cfg: TenantConfig): string {
    if (cfg.managedBy === 'file') return 'This tenant is declared in the configuration file; change it there.';
    if (!management()) return MANAGEMENT_OFF;
    if (!cfg.editable) return 'You can view this configuration but not change it.';
    return '';
  }
</script>

<StateView {res} retry={() => res.load()}>
  {#snippet children(cfg)}
    {@const reason = readOnlyReason(cfg)}
    <section class="panel" aria-labelledby="admin-config">
      <header class="panel-head">
        <h2 id="admin-config">Configuration</h2>
        <span class="small muted">{cfg.managedBy}{cfg.revision !== null ? ` · revision ${cfg.revision}` : ''}</span>
      </header>
      {#if reason}
        <p class="notice" role="note">{reason}</p>
        <SpecView spec={cfg.spec} />
      {:else}
        <div class="panel-body">
          {#key epoch}
            <ConfigEditor
              initial={cfg.spec}
              operator={isOperator()}
              {saving}
              {errMessage}
              {errPath}
              bind:dirty
              onsave={(spec) => save(cfg, spec)}
            >
              {#snippet alertAction()}
                {#if conflict}<button type="button" class="btn" onclick={reload}>Reload the latest (discards your edits)</button>{/if}
              {/snippet}
            </ConfigEditor>
          {/key}
        </div>
      {/if}
    </section>
  {/snippet}
</StateView>

<GeneratedSecrets bind:generated />
