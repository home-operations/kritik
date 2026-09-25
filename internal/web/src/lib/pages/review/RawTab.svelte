<script lang="ts">
  import { getJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import { pretty, bytes } from '../../format';
  import type { ReviewDetail, ReviewRaw } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import CodeBlock from '../../components/CodeBlock.svelte';
  import Collapsible from '../../components/Collapsible.svelte';

  let { base, d, version }: { base: string; d: ReviewDetail; version: number } = $props();
  const res = new Resource(() => getJSON<ReviewRaw>(`${base}/raw`));
  $effect(() => {
    void version;
    void res.load();
  });
  const pack = $derived(d.contextPack);
</script>

<StateView {res} retry={() => res.load()}>
  {#snippet children(x)}
    <section class="panel" aria-labelledby="raw-result">
      <header class="panel-head"><h2 id="raw-result">Submitted result</h2></header>
      {#if x.result === null}<p class="state-msg">Nothing was submitted.</p>{:else}<CodeBlock text={pretty(x.result)} label="result.json" />{/if}
    </section>

    <section class="panel" aria-labelledby="raw-log">
      <header class="panel-head"><h2 id="raw-log">Log tail</h2></header>
      {#if x.logTail}<CodeBlock text={x.logTail} label="runner log" maxLines={80} />{:else}<p class="state-msg">No log captured.</p>{/if}
    </section>

    <section class="panel" aria-labelledby="raw-pack">
      <header class="panel-head"><h2 id="raw-pack">Context pack</h2></header>
      {#if pack}
        <dl class="deflist">
          <dt>Head / base</dt><dd class="mono">{pack.headSha} / {pack.baseSha}</dd>
          <dt>Patch id</dt><dd class="mono">{pack.patchId}</dd>
          {#if pack.priorHeadSha}<dt>Prior head</dt><dd class="mono">{pack.priorHeadSha}</dd>{/if}
          <dt>Changed paths</dt><dd class="mono small">{pack.changedPaths.join(', ') || '—'}</dd>
          {#if pack.deltaPaths.length}<dt>Delta paths</dt><dd class="mono small">{pack.deltaPaths.join(', ')}</dd>{/if}
        </dl>
        {#if pack.repoNotes.length}
          <h3 class="subhead">Notes</h3>
          <ul class="plain-list">{#each pack.repoNotes as n, i (i)}<li class="small">{n}</li>{/each}</ul>
        {/if}
      {/if}
      <h3 class="subhead">Stages ({x.stages.length})</h3>
      {#each x.stages as s, i (i)}
        <Collapsible title="{s.stage} · {s.path}:{s.startLine}-{s.endLine}">
          {#snippet meta()}<span class="mono small muted">{s.kind} {s.symbol}</span>{/snippet}
          <CodeBlock text={s.text} label={s.language || undefined} />
        </Collapsible>
      {:else}
        <p class="state-msg">No stages.</p>
      {/each}
    </section>

    <section class="panel" aria-labelledby="raw-files">
      <header class="panel-head"><h2 id="raw-files">Repository files</h2></header>
      {#each Object.entries(x.repoFiles) as [path, text] (path)}
        <Collapsible title={path}>
          {#snippet meta()}<span class="small muted">{bytes(text.length)}</span>{/snippet}
          <CodeBlock {text} label={path} />
        </Collapsible>
      {:else}
        <p class="state-msg">No repository files were read.</p>
      {/each}
    </section>
  {/snippet}
</StateView>
