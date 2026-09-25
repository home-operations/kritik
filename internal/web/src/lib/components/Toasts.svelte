<script lang="ts">
  import { toasts, dismiss } from '../toast.svelte';
</script>

<div class="toasts" role="status" aria-live="polite">
  {#each toasts.list as t (t.id)}
    <div class="toast tone-{t.tone}">
      <span>{t.text}</span>
      <button class="btn btn-small" aria-label="Dismiss" onclick={() => dismiss(t.id)}>×</button>
    </div>
  {/each}
</div>
