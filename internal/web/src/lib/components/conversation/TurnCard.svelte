<script lang="ts">
  import type { Turn } from '../../types';
  import { duration, pretty, tokens, usd, wholeNumber } from '../../format';
  import Collapsible from '../Collapsible.svelte';
  import CodeBlock from '../CodeBlock.svelte';
  import MessageView from './MessageView.svelte';
  import ToolCallView from './ToolCallView.svelte';
  import ToolDefs from './ToolDefs.svelte';

  let { turn, toolNames }: { turn: Turn; toolNames: Map<string, string> } = $props();
  let raw = $state(false);
  const u = $derived(turn.usage);
</script>

<article class="turn" class:turn-error={!!turn.error} aria-labelledby="turn-{turn.id}">
  <header class="turn-head">
    <h3 id="turn-{turn.id}" class="turn-title">#{turn.index}</h3>
    <span class="badge">{turn.kind.replace('_', ' ')}{turn.kind === 'agent_step' ? ` ${turn.step}` : ''}</span>
    <span class="mono small">{turn.model}</span>
    {#if turn.upstream}<span class="mono small muted">via {turn.upstream}</span>{/if}
    {#if turn.reset}<span class="badge" title="The request was sent in full rather than as new messages">full request</span>{/if}
    {#if turn.truncated}<span class="badge badge-warn" title="The recorded request or response was truncated">truncated</span>{/if}
    <span class="spacer"></span>
    <span class="small muted turn-usage" title="input / cache read / cache write / output tokens">
      {tokens(u.input)} in · {tokens(u.cacheRead)} cache read · {tokens(u.cacheWrite)} cache write · {tokens(u.output)} out
    </span>
    <span class="small" title="{wholeNumber(u.input + u.cacheRead + u.cacheWrite + u.output)} tokens">{usd(turn.costUsd)}</span>
    <span class="small muted">{duration(turn.durationMs)}</span>
    <button class="btn btn-small" aria-pressed={raw} onclick={() => (raw = !raw)}>raw JSON</button>
  </header>
  {#if turn.error}<p class="error-text turn-error-text" role="note">{turn.error}</p>{/if}

  {#if raw}
    <CodeBlock text={pretty(turn)} label="turn {turn.index}" maxLines={200} />
  {:else}
    {#if turn.system !== null}
      <Collapsible title="system prompt changed"><CodeBlock text={turn.system} plain /></Collapsible>
    {/if}
    {#if turn.tools !== null}
      <Collapsible title="tools changed ({turn.tools.length})"><ToolDefs tools={turn.tools} /></Collapsible>
    {/if}
    {#each turn.messages as m, i (i)}<MessageView {m} {toolNames} />{/each}
    <div class="message role-assistant response">
      <span class="role-label">response{turn.response.stop ? ` · ${turn.response.stop}` : ''}</span>
      {#if turn.response.text}<CodeBlock text={turn.response.text} plain />{/if}
      {#each turn.response.toolCalls as c, i (i)}<ToolCallView call={c} />{/each}
      {#if !turn.response.text && turn.response.toolCalls.length === 0}<p class="small muted">No content.</p>{/if}
    </div>
  {/if}
</article>
