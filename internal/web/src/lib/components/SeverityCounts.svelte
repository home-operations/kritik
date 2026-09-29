<script lang="ts">
  // A review's findings by severity as one small tinted square each,
  // blocking first; a severity with none is left out.
  import type { SeverityCounts } from '../types';
  import { SEVERITIES } from '../format';
  let { counts }: { counts: SeverityCounts } = $props();
</script>

<span class="sev-counts">
  {#each SEVERITIES as s (s)}
    {#if counts[s] > 0}
      <span class="sev-chip sev-{s}" title="{counts[s]} {s}">{counts[s]}<span class="sr-only">{` ${s} `}</span></span>
    {/if}
  {/each}
</span>
