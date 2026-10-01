<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { repoRoute, rerunPath, accountApi, threadUrl } from '../links';
  import { isAdmin } from '../session.svelte';
  import ActionButton from '../components/ActionButton.svelte';
  import { shortSha, SEVERITIES } from '../format';
  import { safeHref } from '../markdown';
  import type { PullDetail, ReviewDetail } from '../types';
  import StateView from '../components/StateView.svelte';
  import Time from '../components/Time.svelte';
  import Icon from '../Icon.svelte';
  import { mdiOpenInNew, mdiSourceMerge, mdiSourceBranchRemove, mdiFileDocumentEditOutline, mdiSourcePull } from '../icons';
  import ReviewStatusTile from '../components/ReviewStatusTile.svelte';
  import ReviewMeta from '../components/ReviewMeta.svelte';
  import FollowupItem from '../components/FollowupItem.svelte';
  import Markdown from '../components/Markdown.svelte';

  let { slug, owner, repo, number }: { slug: string; owner: string; repo: string; number: number } = $props();
  const fullName = $derived(`${owner}/${repo}`);

  const res = new Resource(() =>
    getJSON<PullDetail>(`${accountApi(slug)}/pulls/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/${number}`),
  );
  // The newest review in full: what it said leads the page.
  const latestId = $derived(res.data?.reviews[0]?.id);
  const latest = new Resource(() => getJSON<ReviewDetail>(`${accountApi(slug)}/reviews/${encodeURIComponent(latestId ?? '')}`));
  $effect(() => {
    void res.load();
  });
  $effect(() => {
    if (latestId) void latest.load();
  });
  $effect(() =>
    live(
      (e) => e.account === slug && e.kind !== 'index_run' && e.kind !== 'model_call',
      () => {
        void res.load();
        if (latestId) void latest.load();
      },
    ),
  );

  // Label colours come from the forge; anything but a hex triplet/quad/etc.
  // falls back to the border colour rather than reaching the style attribute.
  function labelColor(c: string): string | undefined {
    return /^[0-9a-f]{3,8}$/i.test(c) ? `#${c}` : undefined;
  }

  const skipText: Record<string, string> = {
    disabled: 'reviews disabled',
    filtered: 'excluded by filter',
    only_skipped_paths: 'only ignored paths changed',
  };

  function lifecycle(p: PullDetail['pull']): { icon: string; label: string; tone: string } {
    if (p.merged) return { icon: mdiSourceMerge, label: 'Merged', tone: 'merged' };
    if (p.state === 'closed') return { icon: mdiSourceBranchRemove, label: 'Closed', tone: 'danger' };
    if (p.draft) return { icon: mdiFileDocumentEditOutline, label: 'Draft', tone: 'muted' };
    return { icon: mdiSourcePull, label: 'Open', tone: 'ok' };
  }
</script>

<svelte:head><title>{res.data ? `${res.data.pull.title} · ` : ''}{fullName}#{number} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        {@const p = d.pull}
        {@const forgeUrl = safeHref(p.url)}
        {@const life = lifecycle(p)}
        <header class="page-head">
          <p class="crumbs">
            <a href={href({ name: 'pulls', slug })}>Pull requests</a> /
            <a class="mono" href={href(repoRoute(slug, fullName))}>{fullName}</a>
          </p>
          <h1>{p.title} <span class="muted">#{p.number}</span></h1>
          <p class="meta-line">
            <span class="lifecycle-badge tone-{life.tone}"><Icon path={life.icon} size={13} /> {life.label}</span>
            <span><strong>{p.author}</strong> wants to merge <span class="mono">{p.headRef}</span> into <span class="mono">{p.baseRef}</span></span>
            <span>at <span class="mono" title={p.headSha}>{shortSha(p.headSha)}</span>, updated <Time iso={p.updatedAt} /></span>
            {#each p.labels as l (l.name)}<span class="label-chip" style:--label={labelColor(l.color)}>{l.name}</span>{/each}
            {#if forgeUrl}
              <a class="external" href={forgeUrl} target="_blank" rel="noopener noreferrer">View on GitHub <Icon path={mdiOpenInNew} size={12} /></a>
            {/if}
          </p>
          {#if isAdmin()}
            <div class="page-actions">
              <ActionButton
                label="Re-run"
                title="Re-run the review?"
                body={`Queue a fresh review of ${fullName}#${p.number} at its current head.`}
                path={rerunPath(slug, { repository: fullName, number: p.number })}
                done="Re-run queued"
                ondone={() => res.load()}
              />
            </div>
          {/if}
        </header>

        {#if d.reviews.length === 0}
          <p class="state-msg">Not reviewed yet.</p>
        {:else}
          {@const r = d.reviews[0]!}
          <section class="panel" aria-labelledby="pull-latest">
            <header class="panel-head">
              <h2 id="pull-latest">Latest review</h2>
              <a class="small" href={href({ name: 'review', slug, id: r.id })}>Open the review</a>
            </header>
            <div class="panel-body latest-review">
              <p class="meta-line">
                <ReviewStatusTile status={r.status} />
                <span>started <Time iso={r.createdAt} /></span>
                {#if r.status === 'skipped' && r.skipReason}<span>skipped: {skipText[r.skipReason] ?? r.skipReason}</span>{/if}
              </p>
              {#if r.error}<p class="error-text">{r.error}</p>{/if}
              {#if latest.data && latest.data.review.id === r.id}
                {@const ld = latest.data}
                {#if ld.summary}<div class="latest-take"><Markdown text={ld.summary.take} /></div>{/if}
                {#if ld.findings.length}
                  <ul class="finding-list" aria-label="Findings">
                    {#each SEVERITIES.flatMap((s) => ld.findings.filter((f) => f.severity === s)) as f (f.id)}
                      <li>
                        <span class="sev sev-{f.severity}">{f.severity}</span>
                        <a href={href({ name: 'review', slug, id: r.id })}>{f.title}</a>
                        <span class="mono small muted">{f.path}:{f.line}</span>
                        {#if threadUrl(p.url, f.forgeCommentId)}
                          <a class="external small" href={threadUrl(p.url, f.forgeCommentId)} target="_blank" rel="noopener noreferrer">
                            Thread <Icon path={mdiOpenInNew} size={11} />
                          </a>
                        {/if}
                      </li>
                    {/each}
                  </ul>
                {:else if r.status === 'completed'}
                  <p class="small muted">No findings.</p>
                {/if}
              {/if}
            </div>
          </section>

          <section class="panel" aria-labelledby="pull-reviews">
            <header class="panel-head"><h2 id="pull-reviews">Review history</h2></header>
            <ol class="timeline">
              {#each d.reviews as r (r.id)}
                <li class="timeline-item">
                  <a class="timeline-link" href={href({ name: 'review', slug, id: r.id })}>
                    <span class="timeline-top">
                      <ReviewStatusTile status={r.status} />
                      <Time iso={r.createdAt} />
                      {#if r.status === 'skipped' && r.skipReason}<span class="small muted">skipped: {skipText[r.skipReason] ?? r.skipReason}</span>{/if}
                    </span>
                    <ReviewMeta {r} />
                    {#if r.error}<span class="error-text">{r.error}</span>{/if}
                  </a>
                </li>
              {/each}
            </ol>
          </section>
        {/if}

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
