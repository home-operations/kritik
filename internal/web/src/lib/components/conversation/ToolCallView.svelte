<script lang="ts">
  import type { ToolCall } from '../../types';
  import { pretty } from '../../format';
  import CodeBlock from '../CodeBlock.svelte';
  let { call }: { call: ToolCall } = $props();
</script>

<div class="tool-call">
  <div class="tool-call-head">
    <span class="badge">tool call</span>
    <span class="mono tool-name">{call.name}</span>
    <span class="mono small muted">{call.id}</span>
  </div>
  <CodeBlock text={pretty(call.input)} label="input" />
</div>
