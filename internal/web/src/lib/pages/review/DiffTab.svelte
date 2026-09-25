<script lang="ts">
  import { getJSON } from '../../api.svelte';
  import { Resource } from '../../resource.svelte';
  import { parseDiff } from '../../diff';
  import type { ReviewDetail, ReviewDiff } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import FindingCard from './FindingCard.svelte';
  import DiffFileView from './DiffFileView.svelte';

  // Files this long start collapsed, as does every file once the diff as a
  // whole is this long, so a huge diff doesn't render thousands of rows up front.
  const FILE_LINES = 500;
  const TOTAL_LINES = 5000;

  // The diff is fixed once a review starts, so unlike the other tabs it is
  // not refetched on live updates.
  let { base, d }: { base: string; d: ReviewDetail } = $props();
  const res = new Resource(() => getJSON<ReviewDiff>(`${base}/diff`));
  let delta = $state(false);
  $effect(() => {
    void res.load();
  });

  const files = $derived(res.data ? parseDiff(delta && res.data.deltaDiff ? res.data.deltaDiff : res.data.diff) : []);
  const total = $derived(files.reduce((n, f) => n + f.lines.length, 0));
  const outside = $derived.by(() => {
    const paths = new Set(files.map((f) => f.path));
    return d.findings.filter((f) => !paths.has(f.path));
  });
</script>

<StateView {res} retry={() => res.load()} isEmpty={(x) => !x.diff && !x.deltaDiff} empty="No diff recorded for this review.">
  {#snippet children(x)}
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
    {#each files as file, i (`${delta}:${i}`)}
      <DiffFileView
        {file}
        findings={d.findings.filter((f) => f.path === file.path)}
        initiallyOpen={total <= TOTAL_LINES && file.lines.length <= FILE_LINES}
      />
    {/each}
  {/snippet}
</StateView>
