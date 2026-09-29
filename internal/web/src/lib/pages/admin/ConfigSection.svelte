<script lang="ts">
  import { accountApi } from '../../links';
  import { getJSON } from '../../api.svelte';
  import { setLeaveGuard } from '../../router.svelte';
  import { Resource } from '../../resource.svelte';
  import { MANAGEMENT_OFF, management } from '../../session.svelte';
  import type { AccountConfig } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import ConfigEditor from './ConfigEditor.svelte';
  import SpecView from './SpecView.svelte';
  import { SpecSave } from './save.svelte';

  let { slug }: { slug: string } = $props();
  const path = $derived(`${accountApi(slug)}/config`);
  const res = new Resource(() => getJSON<AccountConfig>(path));
  $effect(() => {
    void res.load();
  });

  const save = new SpecSave(() => path, () => res.load());

  $effect(() => {
    setLeaveGuard(() => save.dirty);
    return () => setLeaveGuard(undefined);
  });

  function readOnlyReason(cfg: AccountConfig): string {
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
        <span class="small muted">revision {cfg.revision}</span>
      </header>
      {#if reason}
        <p class="notice" role="note">{reason}</p>
        <SpecView spec={cfg.spec} />
      {:else}
        <div class="panel-body">
          {#key save.epoch}
            <ConfigEditor
              initial={cfg.spec}
              inherited={cfg.inherited}
              saving={save.saving}
              errMessage={save.errMessage}
              errPath={save.errPath}
              errSeq={save.errSeq}
              bind:dirty={save.dirty}
              onsave={(spec) => save.put({ revision: cfg.revision, spec })}
            >
              {#snippet alertAction()}
                {#if save.conflict}<button type="button" class="btn" onclick={() => save.reload()}>Reload the latest (discards your edits)</button>{/if}
              {/snippet}
            </ConfigEditor>
          {/key}
        </div>
      {/if}
    </section>
  {/snippet}
</StateView>
