<script lang="ts">
  // One row per pull request: its title over its repository, number and
  // author; its last review's findings and status; when it last changed.
  // `selected` marks the keyboard cursor; a click anywhere on a row that is
  // not a link opens the pull request. With onpick, each row has a checkbox
  // for a bulk action, picked naming the ones checked by pullKey.
  import { href, navigate } from '../router.svelte';
  import { pullKey, pullRoute } from '../links';
  import type { Pull } from '../types';
  import Icon from '../Icon.svelte';
  import { mdiSourceMerge, mdiSourceBranchRemove, mdiFileDocumentEditOutline } from '../icons';
  import Time from './Time.svelte';
  import ReviewStatusTile from './ReviewStatusTile.svelte';
  import SeverityCounts from './SeverityCounts.svelte';

  let {
    slug,
    items,
    selected = -1,
    picked = [],
    onpick,
  }: { slug: string; items: Pull[]; selected?: number; picked?: string[]; onpick?: (keys: string[], on: boolean) => void } = $props();

  // lifecycle marks a pull request that is no longer simply open.
  function lifecycle(p: Pull): { icon: string; label: string; tone: string } | undefined {
    if (p.merged) return { icon: mdiSourceMerge, label: 'merged', tone: 'merged' };
    if (p.state === 'closed') return { icon: mdiSourceBranchRemove, label: 'closed', tone: 'muted' };
    if (p.draft) return { icon: mdiFileDocumentEditOutline, label: 'draft', tone: 'muted' };
    return undefined;
  }

  function onRowClick(e: MouseEvent, p: Pull): void {
    if ((e.target as Element).closest('a, button, input, label') || getSelection()?.toString()) return;
    navigate(pullRoute(slug, p));
  }
</script>

<div class="table-wrap table-card">
  <table class="data pull-table">
    <thead>
      <tr>
        {#if onpick}
          <th scope="col" class="pick">
            <input
              type="checkbox"
              aria-label="Select every pull request shown"
              checked={items.length > 0 && items.every((p) => picked.includes(pullKey(p)))}
              onchange={(e) => onpick(items.map(pullKey), e.currentTarget.checked)}
            />
          </th>
        {/if}
        <th scope="col">Pull request</th>
        <th scope="col" title="Blocking, important and nit, in that order">Findings</th>
        <th scope="col">Last review</th>
        <th scope="col" class="num">Updated</th>
      </tr>
    </thead>
    <tbody class="pull-rows">
      {#each items as p, i (p.url)}
        {@const life = lifecycle(p)}
        <!-- The title is the row's link; the click is a larger target for a pointer. -->
        <tr class="pull-row" class:selected={i === selected} data-index={i} onclick={(e) => onRowClick(e, p)}>
          {#if onpick}
            <td class="pick">
              <input
                type="checkbox"
                aria-label="Select {p.repository}#{p.number}"
                checked={picked.includes(pullKey(p))}
                onchange={(e) => onpick([pullKey(p)], e.currentTarget.checked)}
              />
            </td>
          {/if}
          <td class="pull-main">
            <a class="pull-title" href={href(pullRoute(slug, p))} aria-current={i === selected ? 'true' : undefined}>
              {#if life}<span class="lifecycle tone-{life.tone}" title={life.label}><Icon path={life.icon} size={13} label={life.label} /></span>{/if}
              <span class="pull-text">{p.title}</span>
            </a>
            <span class="pull-sub"><span class="mono">{p.repository}</span> · #{p.number} · {p.author}</span>
          </td>
          <td>
            {#if p.lastReview}
              <SeverityCounts counts={p.lastReview.findings} />
            {/if}
          </td>
          <td>
            {#if p.lastReview}
              <ReviewStatusTile status={p.lastReview.status} title="Last review" />
            {:else if p.fork}
              <span class="small muted" title="A pull request from a fork is reviewed when a maintainer comments &quot;@&lt;bot&gt; review&quot; on it">fork, reviewed on request</span>
            {:else}
              <span class="small muted">not reviewed</span>
            {/if}
          </td>
          <td class="num"><Time iso={p.updatedAt} /></td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>
