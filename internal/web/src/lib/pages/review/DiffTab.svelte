<script lang="ts">
  import { getJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import { parseDiff } from '../../diff';
  import type { ReviewDetail, ReviewDiff } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import FindingCard from './FindingCard.svelte';
  import DiffFileView from './DiffFileView.svelte';

  let { base, d, version }: { base: string; d: ReviewDetail; version: number } = $props();
  const res = new Resource(() => getJSON<ReviewDiff>(`${base}/diff`));
  let delta = $state(false);
  $effect(() => {
    void version;
    void res.load();
  });
</script>

<StateView {res} retry={() => res.load()} isEmpty={(x) => !x.diff && !x.deltaDiff} empty="No diff recorded for this review.">
  {#snippet children(x)}
    {@const files = parseDiff(delta && x.deltaDiff ? x.deltaDiff : x.diff)}
    {@const paths = new Set(files.map((f) => f.path))}
    {@const outside = d.findings.filter((f) => !paths.has(f.path))}
    <div class="toolbar">
      <span class="small muted">{files.length} file{files.length === 1 ? '' : 's'}</span>
      {#if x.deltaDiff}
        <div class="view-toggle" role="group" aria-label="Diff scope">
          <button class:active={!delta} aria-pressed={!delta} onclick={() => (delta = false)}>Full</button>
          <button class:active={delta} aria-pressed={delta} onclick={() => (delta = true)}>Since last review</button>
        </div>
      {/if}
    </div>
    {#if outside.length}
      <section class="panel" aria-label="Findings outside this diff">
        <header class="panel-head"><h2>Outside this diff</h2></header>
        {#each outside as f (f.id)}<FindingCard {f} />{/each}
      </section>
    {/if}
    {#each files as file, i (i)}
      <DiffFileView {file} findings={d.findings.filter((f) => f.path === file.path)} />
    {/each}
  {/snippet}
</StateView>
