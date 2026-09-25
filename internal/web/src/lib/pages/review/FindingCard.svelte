<script lang="ts">
  import type { Finding } from '../../types';
  import Markdown from '../../components/Markdown.svelte';
  import CodeBlock from '../../components/CodeBlock.svelte';
  import Collapsible from '../../components/Collapsible.svelte';

  let { f, compact = false }: { f: Finding; compact?: boolean } = $props();
  const where = $derived(f.endLine > f.line ? `${f.path}:${f.line}-${f.endLine}` : `${f.path}:${f.line}`);
</script>

<article class="finding sev-edge-{f.severity}" aria-label="{f.severity}: {f.title}">
  <header class="finding-head">
    <span class="sev sev-{f.severity}">{f.severity}</span>
    <span class="finding-title">{f.title}</span>
    {#if !compact}<span class="mono small muted">{where}</span>{/if}
    {#if f.postedInline}<span class="badge" title="Posted as an inline comment on the forge">inline</span>{/if}
  </header>
  {#if f.explanation}<Markdown text={f.explanation} />{/if}
  {#if f.suggestedFix}
    <p class="finding-label">Suggested fix</p>
    <Markdown text={f.suggestedFix} />
  {/if}
  {#if f.replacement}<CodeBlock text={f.replacement} label="replacement" />{/if}
  {#if f.agentPrompt}
    <Collapsible title="Agent prompt"><CodeBlock text={f.agentPrompt} plain /></Collapsible>
  {/if}
</article>
