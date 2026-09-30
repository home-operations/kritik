<script lang="ts">
  // What the instance still lacks before it reviews, each step with what to
  // set in the configuration file.
  import { getJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import { checklist } from '../../setup';
  import type { AdminAccount, SetupStatus } from '../../types';
  import Icon from '../../Icon.svelte';
  import { mdiCheck, mdiCircleOutline } from '../../icons';

  let { accounts }: { accounts: AdminAccount[] } = $props();

  const status = new Resource(() => getJSON<SetupStatus>('/api/v1/admin/setup'));
  $effect(() => {
    void status.load();
  });
  const items = $derived(status.data ? checklist(status.data, accounts) : []);
</script>

{#if items.length}
  <section class="panel" aria-labelledby="op-setup">
    <header class="panel-head">
      <h2 id="op-setup">Setup</h2>
      <span class="small muted">{items.filter((i) => i.done).length} of {items.length} done</span>
    </header>
    <ul class="checklist">
      {#each items as item (item.label)}
        <li class:done={item.done}>
          <span class="check-mark"><Icon path={item.done ? mdiCheck : mdiCircleOutline} size={14} label={item.done ? 'done' : 'to do'} /></span>
          <span class="check-text">
            <span>{item.label}{#if item.optional}{' '}<span class="small muted">(optional)</span>{/if}</span>
            {#if !item.done}<span class="small muted">{item.how}</span>{/if}
          </span>
        </li>
      {/each}
    </ul>
  </section>
{/if}
