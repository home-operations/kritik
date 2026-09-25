<script lang="ts">
  // Preformatted text with a copy button. Anything past maxLines is clipped
  // behind a "show all" toggle so one huge tool result doesn't bury a page.
  import Copy from './Copy.svelte';

  interface Props {
    text: string;
    label?: string;
    maxLines?: number;
    copy?: boolean;
    // plain renders prose (sans, wrapped) instead of code.
    plain?: boolean;
  }
  let { text, label, maxLines = 40, copy = true, plain = false }: Props = $props();
  let expanded = $state(false);

  const lines = $derived(text.split('\n'));
  const clipped = $derived(!expanded && lines.length > maxLines);
  const shown = $derived(clipped ? lines.slice(0, maxLines).join('\n') : text);
</script>

<div class="code-block" class:plain>
  {#if label || copy}
    <div class="code-head">
      {#if label}<span class="code-label mono">{label}</span>{/if}
      <span class="spacer"></span>
      {#if copy}<Copy {text} />{/if}
    </div>
  {/if}
  <pre class:mono={!plain}>{shown}</pre>
  {#if lines.length > maxLines}
    <button class="btn btn-small code-more" aria-expanded={expanded} onclick={() => (expanded = !expanded)}>
      {expanded ? 'Show less' : `Show all ${lines.length} lines`}
    </button>
  {/if}
</div>
