<script lang="ts">
  // The full model conversation behind a review or follow-up: the base
  // system prompt and tools, then every recorded model call in order.
  import type { Transcript, Turn } from '../../types';
  import Collapsible from '../Collapsible.svelte';
  import CodeBlock from '../CodeBlock.svelte';
  import ToolDefs from './ToolDefs.svelte';
  import TurnCard from './TurnCard.svelte';

  let { transcript }: { transcript: Transcript } = $props();
  let q = $state('');

  function lines(s: string): string {
    const n = s.split('\n').length;
    return n === 1 ? '1 line' : `${n} lines`;
  }

  const toolNames = $derived.by(() => {
    const m = new Map<string, string>();
    for (const t of transcript.turns) {
      for (const c of t.response.toolCalls) m.set(c.id, c.name);
      for (const msg of t.messages) for (const c of msg.toolCalls) m.set(c.id, c.name);
    }
    return m;
  });

  function haystack(t: Turn): string {
    const parts: string[] = [t.kind, t.model, t.error, t.response.text];
    for (const c of t.response.toolCalls) parts.push(c.name, JSON.stringify(c.input));
    for (const m of t.messages) {
      parts.push(m.text);
      for (const c of m.toolCalls) parts.push(c.name, JSON.stringify(c.input));
      for (const r of m.toolResults) parts.push(r.content);
    }
    return parts.join('\n').toLowerCase();
  }

  // Built once per transcript, not per keystroke.
  const haystacks = $derived(transcript.turns.map(haystack));

  const shown = $derived.by(() => {
    const needle = q.trim().toLowerCase();
    return needle ? transcript.turns.filter((_, i) => haystacks[i]!.includes(needle)) : transcript.turns;
  });
</script>

<div class="conversation">
  <div class="conversation-top">
    {#if transcript.system}
      <Collapsible title="System prompt">
        {#snippet meta()}<span class="small muted">{lines(transcript.system)}</span>{/snippet}
        <CodeBlock text={transcript.system} plain />
      </Collapsible>
    {/if}
    {#if transcript.tools.length}
      <Collapsible title="Tools ({transcript.tools.length})">
        {#snippet meta()}<span class="small muted mono">{transcript.tools.map((t) => t.name).join(', ')}</span>{/snippet}
        <ToolDefs tools={transcript.tools} />
      </Collapsible>
    {/if}
  </div>
  <div class="toolbar" role="search">
    <label class="search-box">
      <span class="sr-only">Filter turns</span>
      <input type="search" placeholder="Filter turns by text" bind:value={q} />
    </label>
    <span class="small muted">{shown.length} of {transcript.turns.length} turns</span>
  </div>
  {#if shown.length === 0}
    <p class="state-msg">No turn matches “{q}”.</p>
  {/if}
  {#each shown as t (t.id)}<TurnCard turn={t} {toolNames} />{/each}
</div>
