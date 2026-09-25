<script lang="ts">
  // Shows the webhook secrets a save generated, exactly once: closing the
  // dialog drops them, and the API never returns them again.
  import { hookName } from '../../spec';
  import Dialog from '../../components/Dialog.svelte';
  import Copy from '../../components/Copy.svelte';

  let { generated = $bindable(), fallback }: { generated: Record<string, string> | undefined; fallback?: string } = $props();
  let open = $state(false);
  $effect(() => {
    open = !!generated && Object.keys(generated).length > 0;
  });
</script>

<Dialog bind:open title="Generated webhook secrets" {fallback} onclose={() => (generated = undefined)}>
  <p class="form-alert" role="note"><strong>This is the only time these are shown.</strong> Copy each into the forge's webhook settings now.</p>
  {#each Object.entries(generated ?? {}) as [key, value] (key)}
    <div class="item-card">
      <div class="item-head"><span class="mono">{key}</span></div>
      <p class="small">Hook path: <span class="mono">/hooks/{hookName(key)}</span></p>
      <div class="secret-once">
        <span class="mono" data-testid="generated-secret">{value}</span>
        <Copy text={value} label="Copy secret" />
      </div>
    </div>
  {/each}
  {#snippet footer()}
    <button class="btn btn-primary" onclick={() => (open = false)}>I have copied them</button>
  {/snippet}
</Dialog>
