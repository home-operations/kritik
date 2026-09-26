<script lang="ts">
  import { Paged, live } from '../resource.svelte';
  import type { Followup } from '../types';
  import StateView from '../components/StateView.svelte';
  import FollowupItem from '../components/FollowupItem.svelte';
  import LoadMore from '../components/LoadMore.svelte';

  let { slug }: { slug: string } = $props();
  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/followups`);
  const paged = new Paged<Followup>(
    (cursor) => `${base}?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
    (f) => f.id,
  );
  const res = paged.first;

  $effect(() => {
    void paged.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind === 'followup', () => void paged.load()));
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Follow-ups</h1></header>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.items.length === 0} empty="No follow-up questions yet.">
      {#snippet children()}
        <ul class="followups">
          {#each paged.items as f (f.id)}<FollowupItem {slug} {f} showPull />{/each}
        </ul>
        <LoadMore {paged} />
      {/snippet}
    </StateView>
  </div>
</main>
