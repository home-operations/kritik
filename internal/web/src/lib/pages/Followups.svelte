<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { Resource, live } from '../resource.svelte';
  import type { Followup, Page } from '../types';
  import StateView from '../components/StateView.svelte';
  import FollowupItem from '../components/FollowupItem.svelte';

  let { slug }: { slug: string } = $props();
  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/followups`);
  const res = new Resource(() => getJSON<Page<Followup>>(`${base}?limit=50`));
  let extra = $state<Followup[]>([]);
  let cursor = $state<string | null>(null);
  let loadingMore = $state(false);

  $effect(() => {
    void res.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind === 'followup', () => void res.load()));
  $effect(() => {
    cursor = res.data?.nextCursor ?? null;
    extra = [];
  });

  async function more(): Promise<void> {
    if (!cursor) return;
    loadingMore = true;
    try {
      const p = await getJSON<Page<Followup>>(`${base}?limit=50&cursor=${encodeURIComponent(cursor)}`);
      extra = [...extra, ...p.items];
      cursor = p.nextCursor;
    } catch (err) {
      console.error('load more follow-ups:', err);
    } finally {
      loadingMore = false;
    }
  }
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Follow-ups</h1></header>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.items.length === 0} empty="No follow-up questions yet.">
      {#snippet children(d)}
        <ul class="followups">
          {#each [...d.items, ...extra] as f (f.id)}<FollowupItem {slug} {f} showPull />{/each}
        </ul>
        {#if cursor}
          <button class="btn load-more" onclick={more} disabled={loadingMore}>{loadingMore ? 'Loading…' : 'Load more'}</button>
        {/if}
      {/snippet}
    </StateView>
  </div>
</main>
