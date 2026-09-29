<script lang="ts">
  // The instance spec's editor in the admin console, or its redacted JSON
  // when management is off.
  import { getJSON } from '../../api.svelte';
  import { setLeaveGuard } from '../../router.svelte';
  import { Resource } from '../../resource.svelte';
  import { isCode } from '../../manage';
  import { MANAGEMENT_OFF, management } from '../../session.svelte';
  import type { InstanceConfig, UpdateConfigRequest } from '../../types';
  import Dialog from '../../components/Dialog.svelte';
  import StateView from '../../components/StateView.svelte';
  import InstanceEditor from './InstanceEditor.svelte';
  import SpecView from './SpecView.svelte';
  import GeneratedSecrets from './GeneratedSecrets.svelte';
  import { SpecSave } from './save.svelte';

  let { onsaved }: { onsaved?: () => void } = $props();
  const path = '/api/v1/config';
  const res = new Resource(() => getJSON<InstanceConfig>(path));
  $effect(() => {
    void res.load();
  });

  const save = new SpecSave(() => path, () => res.load());
  let generated = $state<Record<string, string> | undefined>(undefined);
  // A save the server refused until the admin confirms rebuilding every
  // index, held while the dialog asks.
  let pending = $state<{ cfg: InstanceConfig; spec: Record<string, unknown> } | undefined>(undefined);
  let reindexOpen = $state(false);

  $effect(() => {
    setLeaveGuard(() => save.dirty || generated !== undefined);
    return () => setLeaveGuard(undefined);
  });

  async function write(cfg: InstanceConfig, spec: Record<string, unknown>, reindex = false): Promise<void> {
    const body: UpdateConfigRequest = { revision: cfg.revision, spec };
    if (reindex) body.confirmReindex = true;
    await save.put(body, {
      saved: (r) => {
        if (r.generated && Object.keys(r.generated).length) generated = r.generated;
        onsaved?.();
      },
      refused: (err) => {
        if (!isCode(err, 'reindex_required')) return false;
        pending = { cfg, spec };
        reindexOpen = true;
        return true;
      },
    });
  }

  function confirmReindex(): void {
    const p = pending;
    reindexOpen = false;
    if (p) void write(p.cfg, p.spec, true);
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
          {#key save.epoch}
            <InstanceEditor
              initial={cfg.spec}
              inherited={cfg.inherited}
              saving={save.saving}
              errMessage={save.errMessage}
              errPath={save.errPath}
              errSeq={save.errSeq}
              bind:dirty={save.dirty}
              onsave={(spec) => write(cfg, spec)}
            >
              {#snippet alertAction()}
                {#if save.conflict}<button type="button" class="btn" onclick={() => save.reload()}>Reload the latest (discards your edits)</button>{/if}
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
