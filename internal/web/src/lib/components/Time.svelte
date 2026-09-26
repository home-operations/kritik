<script lang="ts">
  // A relative timestamp ("5m ago") that stays current off the shared clock,
  // with the absolute local time on hover and in the datetime attribute.
  import { clock, timeAgo, absolute } from '../time.svelte';
  let { iso }: { iso: string | null | undefined } = $props();
</script>

{#if iso}
  <time class="reltime" datetime={iso} title={absolute(iso)}>{timeAgo(iso, clock.now)}</time>
{:else}
  <span class="muted">—</span>
{/if}
