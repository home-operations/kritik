<script lang="ts">
  import type { Message } from '../../types';
  import { bytes } from '../../format';
  import Collapsible from '../Collapsible.svelte';
  import CodeBlock from '../CodeBlock.svelte';
  import ToolCallView from './ToolCallView.svelte';

  // toolNames maps a tool-call id to the tool it invoked, so a result can say
  // what it answers even when the call was in an earlier turn.
  let { m, toolNames }: { m: Message; toolNames: Map<string, string> } = $props();
</script>

<div class="message role-{m.role}">
  <span class="role-label">{m.role}</span>
  {#if m.text}<CodeBlock text={m.text} plain copy={false} />{/if}
  {#each m.toolCalls as c, i (i)}<ToolCallView call={c} />{/each}
  {#each m.toolResults as r, i (i)}
    <Collapsible
      title="tool result · {toolNames.get(r.callId) ?? r.callId}"
      tone={r.isError ? 'danger' : ''}
      open={r.isError}
    >
      {#snippet meta()}
        <span class="small muted">{bytes(new TextEncoder().encode(r.content).length)}</span>
        {#if r.isError}<span class="badge badge-danger">error</span>{/if}
        {#if r.truncatedBytes > 0}
          <span class="badge badge-warn" title="The tool output was cut before it reached the model">truncated {bytes(r.truncatedBytes)}</span>
        {/if}
      {/snippet}
      <CodeBlock text={r.content} />
      {#if r.truncatedBytes > 0}
        <p class="small muted truncation-note">… {bytes(r.truncatedBytes)} more were cut before the model saw this result.</p>
      {/if}
    </Collapsible>
  {/each}
</div>
