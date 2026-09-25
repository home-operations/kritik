<script lang="ts">
  import { getJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import type { Transcript } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import Conversation from '../../components/conversation/Conversation.svelte';

  let { base, version }: { base: string; version: number } = $props();
  const res = new Resource(() => getJSON<Transcript>(`${base}/transcript`));
  $effect(() => {
    void version;
    void res.load();
  });
</script>

<StateView {res} retry={() => res.load()} isEmpty={(t) => t.turns.length === 0} empty="No model calls recorded for this review.">
  {#snippet children(t)}
    <Conversation transcript={t} />
  {/snippet}
</StateView>
