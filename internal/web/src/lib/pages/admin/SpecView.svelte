<script lang="ts">
  // A read-only rendering of a tenant spec: nested keys as a tree, and
  // every secret position ({"set": bool}) as "set" or "not set", never a
  // value (the API never sends one).
  let { spec }: { spec: Record<string, unknown> } = $props();

  type Obj = Record<string, unknown>;

  function isObj(v: unknown): v is Obj {
    return typeof v === 'object' && v !== null && !Array.isArray(v);
  }

  function isSecret(v: unknown): v is { set: boolean } {
    return isObj(v) && Object.keys(v).length === 1 && typeof v.set === 'boolean';
  }
</script>

{#snippet node(v: unknown)}
  {#if isSecret(v)}
    <span class="pill tone-{v.set ? 'ok' : 'muted'}">{v.set ? 'set' : 'not set'}</span>
  {:else if Array.isArray(v)}
    {#if v.length === 0}<span class="muted">none</span>{:else}
      <ol class="spec-tree">
        {#each v as item, i (i)}<li>{@render node(item)}</li>{/each}
      </ol>
    {/if}
  {:else if isObj(v)}
    <ul class="spec-tree">
      {#each Object.entries(v) as [k, x] (k)}
        <li><span class="spec-key">{k}:</span> {@render node(x)}</li>
      {/each}
    </ul>
  {:else}
    <span class="mono">{String(v)}</span>
  {/if}
{/snippet}

<div class="panel-body spec-view">{@render node(spec)}</div>
