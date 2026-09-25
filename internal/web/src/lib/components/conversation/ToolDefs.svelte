<script lang="ts">
  import type { ToolDef } from '../../types';
  import { pretty } from '../../format';
  import Collapsible from '../Collapsible.svelte';
  import CodeBlock from '../CodeBlock.svelte';
  let { tools }: { tools: ToolDef[] } = $props();
</script>

<ul class="tool-defs">
  {#each tools as t (t.name)}
    <li>
      <Collapsible title={t.name}>
        {#snippet meta()}<span class="muted">{t.description.split('\n')[0]}</span>{/snippet}
        {#if t.description}<CodeBlock text={t.description} plain copy={false} />{/if}
        <CodeBlock text={pretty(t.inputSchema)} label="input schema" />
      </Collapsible>
    </li>
  {/each}
</ul>
