<script lang="ts">
  import type { Finding } from '../../types';
  import Markdown from '../../components/Markdown.svelte';
  import CodeBlock from '../../components/CodeBlock.svelte';
  import Collapsible from '../../components/Collapsible.svelte';
  import Icon from '../../Icon.svelte';
  import { mdiOpenInNew } from '../../icons';
  import { threadUrl } from '../../links';
  import Reactions from '../../components/Reactions.svelte';

  // pullUrl is the pull request on the forge, where an inline finding's
  // thread is.
  let { f, pullUrl = '', compact = false }: { f: Finding; pullUrl?: string; compact?: boolean } = $props();
  const thread = $derived(pullUrl ? threadUrl(pullUrl, f.forgeCommentId) : undefined);
  const where = $derived(f.endLine > f.line ? `${f.path}:${f.line}-${f.endLine}` : `${f.path}:${f.line}`);
</script>

<article class="finding sev-edge-{f.severity}" aria-label="{f.severity}: {f.title}">
  <header class="finding-head">
    <span class="sev sev-{f.severity}">{f.severity}</span>
    <span class="finding-title">{f.title}</span>
    {#if !compact}<span class="mono small muted">{where}</span>{/if}
    <Reactions up={f.reactionsUp} down={f.reactionsDown} />
    {#if thread}
      <a class="external small finding-thread" href={thread} target="_blank" rel="noopener noreferrer">Thread on GitHub <Icon path={mdiOpenInNew} size={12} /></a>
    {:else if f.postedInline}<span class="badge" title="Posted as an inline comment on GitHub">inline</span>{/if}
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
