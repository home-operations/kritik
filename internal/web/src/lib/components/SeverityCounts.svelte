<script lang="ts">
  import type { SeverityCounts } from '../types';
  import { SEVERITIES } from '../format';
  let { counts }: { counts: SeverityCounts } = $props();
</script>

<span class="sev-counts">
  {#each SEVERITIES as s (s)}
    {#if counts[s] > 0}
      <span class="sev sev-{s}" title="{counts[s]} {s}">{counts[s]} {s}</span>
    {/if}
  {/each}
</span>
