<script lang="ts" generics="T">
  import type { Paged } from '../resource.svelte';
  import { errorMessage } from '../resource.svelte';
  let { paged }: { paged: Paged<T> } = $props();
</script>

{#if paged.moreError}
  <p class="state-msg state-error" role="alert">Loading more failed: {errorMessage(paged.moreError)}</p>
{/if}
{#if paged.cursor}
  <button class="btn load-more" onclick={() => paged.more()} disabled={paged.loadingMore}>
    {paged.loadingMore ? 'Loading…' : 'Load more'}
  </button>
{/if}
