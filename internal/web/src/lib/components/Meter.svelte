<script lang="ts">
  // A usage-vs-limit bar. A limit of 0 means unlimited: no bar, just "no cap".
  let { value, max, label }: { value: number; max: number; label: string } = $props();
  const pct = $derived(max > 0 ? Math.min(100, (value / max) * 100) : 0);
  const tone = $derived(pct >= 90 ? 'danger' : pct >= 70 ? 'warn' : 'ok');
</script>

{#if max > 0}
  <div
    class="meter tone-{tone}"
    role="meter"
    aria-label={label}
    aria-valuemin={0}
    aria-valuemax={max}
    aria-valuenow={value}
  >
    <span class="meter-fill" style:width="{pct}%"></span>
  </div>
{:else}
  <span class="muted small">no cap</span>
{/if}
