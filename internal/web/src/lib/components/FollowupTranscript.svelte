<script lang="ts">
  // Lazily loads and shows one follow-up's model conversation.
  import { getJSON } from '../api.svelte';
  import { Resource } from '../resource.svelte';
  import type { Transcript } from '../types';
  import StateView from './StateView.svelte';
  import Conversation from './conversation/Conversation.svelte';

  let { slug, commentId }: { slug: string; commentId: number } = $props();
  const res = new Resource(() =>
    getJSON<Transcript>(`/api/v1/tenants/${encodeURIComponent(slug)}/followups/${commentId}/transcript`),
  );
  $effect(() => {
    void res.load();
  });
</script>

<StateView {res} retry={() => res.load()} isEmpty={(t) => t.turns.length === 0} empty="No model calls recorded for this follow-up.">
  {#snippet children(t)}
    <Conversation transcript={t} />
  {/snippet}
</StateView>
