<script lang="ts">
  // One setting as a row: its name and what it does on the left, its
  // control on the right, and under the name, when given, what the current
  // choice does. The control names itself by the row's label id.
  import type { Snippet } from 'svelte';
  let { id, label, hint = '', note = '', children }: { id: string; label: string; hint?: string; note?: string; children: Snippet } = $props();
</script>

<div class="setting-row">
  <div class="setting-text">
    <span class="setting-name" {id}>{label}</span>
    {#if hint}<span class="setting-hint">{hint}</span>{/if}
    {#if note}<span class="setting-note">{note}</span>{/if}
  </div>
  <div class="setting-control">{@render children()}</div>
</div>
