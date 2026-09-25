<script lang="ts">
  // One row per pull request: repo#number, title, author, state, last review
  // with findings, relative time. `selected` highlights the keyboard cursor.
  import { href } from '../router.svelte';
  import { pullRoute } from '../links';
  import type { Pull } from '../types';
  import Pill from './Pill.svelte';
  import Time from './Time.svelte';
  import ReviewStatusPill from './ReviewStatusPill.svelte';
  import SeverityCounts from './SeverityCounts.svelte';

  let { slug, items, selected = -1 }: { slug: string; items: Pull[]; selected?: number } = $props();

  function stateOf(p: Pull): { tone: 'ok' | 'merged' | 'muted' | 'danger'; label: string } {
    if (p.merged) return { tone: 'merged', label: 'merged' };
    if (p.state === 'closed') return { tone: 'danger', label: 'closed' };
    if (p.draft) return { tone: 'muted', label: 'draft' };
    return { tone: 'ok', label: 'open' };
  }
</script>

<ul class="rows pull-rows">
  {#each items as p, i (p.repository + '#' + p.number)}
    {@const st = stateOf(p)}
    <li class="row" class:selected={i === selected} data-index={i}>
      <a class="row-link" href={href(pullRoute(slug, p))} aria-current={i === selected ? 'true' : undefined}>
        <span class="mono small muted">{p.repository}#{p.number}</span>
        <span class="row-text">{p.title}</span>
        <span class="small muted">{p.author}</span>
      </a>
      <span class="row-meta">
        <Pill tone={st.tone} label={st.label} />
        {#if p.lastReview}
          <SeverityCounts counts={p.lastReview.findings} />
          <ReviewStatusPill status={p.lastReview.status} title="Last review" />
        {:else}
          <span class="small muted">not reviewed</span>
        {/if}
        <Time iso={p.updatedAt} />
      </span>
    </li>
  {/each}
</ul>
