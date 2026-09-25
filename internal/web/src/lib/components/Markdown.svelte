<script lang="ts">
  // Renders the safe markdown subset from markdown.ts. Every value goes
  // through text interpolation (auto-escaped); links are http(s) only.
  import { parseMarkdown } from '../markdown';
  import CodeBlock from './CodeBlock.svelte';
  let { text }: { text: string } = $props();
  const blocks = $derived(parseMarkdown(text));
</script>

<div class="md">
  {#each blocks as b, i (i)}
    {#if b.kind === 'code'}
      <CodeBlock text={b.text} label={b.lang || undefined} />
    {:else}
      <p>
        {#each b.inlines as t, j (j)}
          {#if t.kind === 'code'}<code>{t.text}</code>{:else if t.kind === 'bold'}<strong>{t.text}</strong>{:else if t.kind === 'link'}<a
              href={t.href}
              target="_blank"
              rel="noopener noreferrer">{t.text}</a
            >{:else}{t.text}{/if}
        {/each}
      </p>
    {/if}
  {/each}
</div>
