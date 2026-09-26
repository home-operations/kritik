<script lang="ts">
  import type { ReviewDetail, Severity } from '../../types';
  import { SEVERITIES } from '../../format';
  import Markdown from '../../components/Markdown.svelte';
  import FindingCard from './FindingCard.svelte';

  let { d }: { d: ReviewDetail } = $props();
  const groups = $derived(
    SEVERITIES.map((s: Severity) => ({ s, items: d.findings.filter((f) => f.severity === s) })).filter((g) => g.items.length),
  );
</script>

{#if d.summary}
  <section class="panel" aria-labelledby="sum-take">
    <header class="panel-head"><h2 id="sum-take">Take</h2></header>
    <div class="panel-body"><Markdown text={d.summary.take} /></div>
    {#if d.summary.praise.length}
      <h3 class="subhead">Praise</h3>
      <ul class="praise">
        {#each d.summary.praise as p, i (i)}<li><Markdown text={p} /></li>{/each}
      </ul>
    {/if}
  </section>
{:else}
  <p class="state-msg">No summary{d.review.status === 'running' || d.review.status === 'prepared' ? ' yet' : ''}.</p>
{/if}

{#if groups.length === 0}
  <p class="state-msg">No findings.</p>
{/if}
{#each groups as g (g.s)}
  <section class="finding-group" aria-labelledby="sev-{g.s}">
    <h2 id="sev-{g.s}" class="finding-group-head"><span class="sev sev-{g.s}">{g.s}</span> {g.items.length}</h2>
    {#each g.items as f (f.id)}<FindingCard {f} />{/each}
  </section>
{/each}
