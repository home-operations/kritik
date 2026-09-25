<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { repoRoute } from '../links';
  import { shortSha } from '../format';
  import type { PullDetail } from '../types';
  import StateView from '../components/StateView.svelte';
  import Time from '../components/Time.svelte';
  import Icon from '../Icon.svelte';
  import { mdiOpenInNew } from '../icons';
  import ReviewStatusPill from '../components/ReviewStatusPill.svelte';
  import ReviewMeta from '../components/ReviewMeta.svelte';
  import FollowupItem from '../components/FollowupItem.svelte';

  let { slug, owner, repo, number }: { slug: string; owner: string; repo: string; number: number } = $props();
  const fullName = $derived(`${owner}/${repo}`);

  const res = new Resource(() =>
    getJSON<PullDetail>(
      `/api/v1/tenants/${encodeURIComponent(slug)}/pulls/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${number}`,
    ),
  );
  $effect(() => {
    void res.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind !== 'index_run', () => void res.load()));

  const skipText: Record<string, string> = {
    disabled: 'reviews disabled',
    filtered: 'excluded by filter',
    only_skipped_paths: 'only ignored paths changed',
  };
</script>

<main class="page">
  <div class="page-inner">
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        {@const p = d.pull}
        <header class="page-head">
          <p class="crumbs">
            <a href={href({ name: 'pulls', slug })}>Pull requests</a> /
            <a class="mono" href={href(repoRoute(slug, fullName))}>{fullName}</a>
          </p>
          <h1>{p.title} <span class="muted">#{p.number}</span></h1>
          <p class="meta-line">
            <span>{p.author}</span>
            <span class="mono">{p.headRef} → {p.baseRef}</span>
            <span class="mono" title={p.headSha}>{shortSha(p.headSha)}</span>
            <span>{p.merged ? 'merged' : p.draft ? 'draft' : p.state}</span>
            <span>updated <Time iso={p.updatedAt} /></span>
            {#each p.labels as l (l.name)}<span class="label-chip" style:--label="#{l.color}">{l.name}</span>{/each}
            {#if p.url}
              <a href={p.url} target="_blank" rel="noopener noreferrer">View on forge <Icon path={mdiOpenInNew} size={12} /></a>
            {/if}
          </p>
        </header>

        <section class="panel" aria-labelledby="pull-reviews">
          <header class="panel-head"><h2 id="pull-reviews">Review history</h2></header>
          {#if d.reviews.length === 0}
            <p class="state-msg">Not reviewed yet.</p>
          {:else}
            <ol class="timeline">
              {#each d.reviews as r (r.id)}
                <li class="timeline-item">
                  <a class="timeline-link" href={href({ name: 'review', slug, id: r.id })}>
                    <span class="timeline-top">
                      <ReviewStatusPill status={r.status} />
                      <Time iso={r.createdAt} />
                      {#if r.skipReason}<span class="small muted">skipped: {skipText[r.skipReason] ?? r.skipReason}</span>{/if}
                    </span>
                    <ReviewMeta {r} />
                    {#if r.error}<span class="error-text">{r.error}</span>{/if}
                  </a>
                </li>
              {/each}
            </ol>
          {/if}
        </section>

        <section class="panel" aria-labelledby="pull-followups">
          <header class="panel-head"><h2 id="pull-followups">Follow-ups</h2></header>
          {#if d.followups.length === 0}
            <p class="state-msg">No follow-up questions.</p>
          {:else}
            <ul class="followups">
              {#each d.followups as f (f.id)}<FollowupItem {slug} {f} />{/each}
            </ul>
          {/if}
        </section>
      {/snippet}
    </StateView>
  </div>
</main>
