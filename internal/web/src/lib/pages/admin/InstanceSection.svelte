<script lang="ts">
  // The instance spec's editor in the admin console, or its redacted JSON
  // when management is off.
  import { getJSON, sendJSON } from '../../api.svelte';
  import { setLeaveGuard } from '../../router.svelte';
  import { Resource } from '../../resource.svelte';
  import { describe, errorPath, isCode } from '../../manage';
  import { MANAGEMENT_OFF, management } from '../../session.svelte';
  import { toast } from '../../toast.svelte';
  import type { ConfigWriteResult, InstanceConfig, UpdateConfigRequest } from '../../types';
  import Dialog from '../../components/Dialog.svelte';
  import StateView from '../../components/StateView.svelte';
  import InstanceEditor from './InstanceEditor.svelte';
  import SpecView from './SpecView.svelte';
  import GeneratedSecrets from './GeneratedSecrets.svelte';

  let { onsaved }: { onsaved?: () => void } = $props();
  const path = '/api/v1/config';
  const res = new Resource(() => getJSON<InstanceConfig>(path));
  $effect(() => {
    void res.load();
  });

  let saving = $state(false);
  let dirty = $state(false);
  let errMessage = $state('');
  let errPath = $state('');
  let errSeq = $state(0);
  let conflict = $state(false);
  let generated = $state<Record<string, string> | undefined>(undefined);
  let epoch = $state(0);
  // A save the server refused until the admin confirms rebuilding every
  // index, held while the dialog asks.
  let pending = $state<{ cfg: InstanceConfig; spec: Record<string, unknown> } | undefined>(undefined);
  let reindexOpen = $state(false);

  $effect(() => {
    setLeaveGuard(() => dirty || generated !== undefined);
    return () => setLeaveGuard(undefined);
  });

  async function reload(): Promise<void> {
    errMessage = '';
    errPath = '';
    conflict = false;
    dirty = false;
    await res.load();
    epoch++;
  }

  async function save(cfg: InstanceConfig, spec: Record<string, unknown>, reindex = false): Promise<void> {
    saving = true;
    errMessage = '';
    errPath = '';
    conflict = false;
    const body: UpdateConfigRequest = { revision: cfg.revision, spec };
    if (reindex) body.confirmReindex = true;
    try {
      const r = await sendJSON<ConfigWriteResult>('PUT', path, body);
      toast(`Saved: revision ${r.revision}`);
      if (r.generated && Object.keys(r.generated).length) generated = r.generated;
      await reload();
      onsaved?.();
    } catch (err) {
      if (isCode(err, 'reindex_required')) {
        pending = { cfg, spec };
        reindexOpen = true;
        return;
      }
      errMessage = describe(err);
      errPath = errorPath(err);
      errSeq++;
      conflict = isCode(err, 'revision_conflict');
    } finally {
      saving = false;
    }
  }

  function confirmReindex(): void {
    const p = pending;
    reindexOpen = false;
    if (p) void save(p.cfg, p.spec, true);
  }
</script>

<StateView {res} retry={() => res.load()}>
  {#snippet children(cfg)}
    <section class="panel" aria-labelledby="op-config">
      <header class="panel-head">
        <h2 id="op-config">Instance configuration</h2>
        <span class="small muted">revision {cfg.revision}</span>
      </header>
      {#if !management() || !cfg.editable}
        <p class="notice" role="note">{MANAGEMENT_OFF}</p>
        <SpecView spec={cfg.spec} />
      {:else}
        <div class="panel-body">
          {#key epoch}
            <InstanceEditor initial={cfg.spec} {saving} {errMessage} {errPath} {errSeq} bind:dirty onsave={(spec) => save(cfg, spec)}>
              {#snippet alertAction()}
                {#if conflict}<button type="button" class="btn" onclick={reload}>Reload the latest (discards your edits)</button>{/if}
              {/snippet}
            </InstanceEditor>
          {/key}
        </div>
      {/if}
    </section>
  {/snippet}
</StateView>

<GeneratedSecrets bind:generated fallback="#op-config" />

<Dialog bind:open={reindexOpen} title="Rebuild every index?" onclose={() => (pending = undefined)} fallback="#op-config">
  <p>
    The index was built with another embedding model or dimension. Saving drops every repository's index and builds each
    again from scratch, which embeds every repository anew; reviews run without similar code until theirs is rebuilt.
  </p>
  {#snippet footer()}
    <button class="btn" onclick={() => (reindexOpen = false)}>Keep editing</button>
    <button class="btn btn-primary btn-danger" onclick={confirmReindex}>Save and reindex</button>
  {/snippet}
</Dialog>
