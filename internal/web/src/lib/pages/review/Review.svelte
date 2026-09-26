<script lang="ts">
  import { getJSON } from '../../api.svelte';
  import { href } from '../../router.svelte';
  import { REVIEW_TABS, type ReviewTab } from '../../routes';
  import { Resource, live } from '../../resource.svelte';
  import { isActive } from '../../format';
  import { pullRoute, rerunPath, cancelPath } from '../../links';
  import { canAdmin } from '../../session.svelte';
  import ActionButton from '../../components/ActionButton.svelte';
  import type { ReviewDetail } from '../../types';
  import StateView from '../../components/StateView.svelte';
  import ReviewStatusPill from '../../components/ReviewStatusPill.svelte';
  import ReviewMeta from '../../components/ReviewMeta.svelte';
  import Time from '../../components/Time.svelte';
  import SummaryTab from './SummaryTab.svelte';
  import DiffTab from './DiffTab.svelte';
  import ConversationTab from './ConversationTab.svelte';
  import TimelineTab from './TimelineTab.svelte';
  import RawTab from './RawTab.svelte';
  import UsageTab from './UsageTab.svelte';

  let { slug, id, tab = 'summary' }: { slug: string; id: string; tab?: ReviewTab } = $props();

  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/reviews/${encodeURIComponent(id)}`);
  const res = new Resource(() => getJSON<ReviewDetail>(base));
  // Bumped on every live refresh so the lazily-loaded tab data refetches too.
  let version = $state(0);

  $effect(() => {
    void res.load();
  });
  $effect(() =>
    live(
      (e) => e.tenant === slug && e.reviewId === id && !!res.data && isActive(res.data.review.status),
      () => {
        void res.load();
        version++;
      },
    ),
  );

  const labels: Record<ReviewTab, string> = {
    summary: 'Summary',
    diff: 'Diff',
    conversation: 'Conversation',
    timeline: 'Timeline',
    raw: 'Raw',
    usage: 'Usage',
  };
</script>

<main class="page">
  <div class="page-inner">
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        {@const r = d.review}
        <header class="page-head">
          <p class="crumbs">
            <a href={href({ name: 'pulls', slug })}>Pull requests</a> /
            <a class="mono" href={href(pullRoute(slug, r.pull))}>{r.pull.repository}#{r.pull.number}</a>
          </p>
          <h1>{r.pull.title}</h1>
          <p class="meta-line">
            <ReviewStatusPill status={r.status} />
            <Time iso={r.createdAt} />
            <ReviewMeta {r} />
          </p>
          {#if r.scopeReason}<p class="small muted">scope: {r.scope} ({r.scopeReason})</p>{/if}
          {#if r.priorReviewId}
            <p class="small muted">
              follows <a class="mono" href={href({ name: 'review', slug, id: r.priorReviewId })}>{r.priorReviewId}</a>
            </p>
          {/if}
          {#if r.error}<p class="error-text" role="note">{r.error}</p>{/if}
          {#if r.cancelRequestedAt}<p class="small muted">cancel requested <Time iso={r.cancelRequestedAt} /></p>{/if}
          {#if canAdmin(slug)}
            <div class="page-actions">
              <ActionButton
                label="Re-run"
                title="Re-run the review?"
                body={`Queue a fresh review of ${r.pull.repository}#${r.pull.number} at its current head.`}
                path={rerunPath(slug, r.pull)}
                done="Re-run queued"
                ondone={() => res.load()}
              />
              {#if isActive(r.status) && !r.cancelRequestedAt}
                <ActionButton
                  label="Cancel review"
                  title="Cancel this review?"
                  body="The run stops at its next checkpoint and nothing more is posted to the pull request."
                  path={cancelPath(slug, id)}
                  danger
                  done="Cancel requested"
                  ondone={() => res.load()}
                />
              {/if}
            </div>
          {/if}
        </header>

        <nav class="tabs" aria-label="Review sections">
          {#each REVIEW_TABS as t (t)}
            <a class="tab" class:active={t === tab} aria-current={t === tab ? 'page' : undefined} href={href({ name: 'review', slug, id, tab: t })}>
              {labels[t]}{#if t === 'summary' && d.findings.length}<span class="tab-count">{d.findings.length}</span>{/if}
            </a>
          {/each}
        </nav>

        <section class="tab-panel" aria-label={labels[tab]}>
          {#if tab === 'summary'}
            <SummaryTab {d} />
          {:else if tab === 'diff'}
            <DiffTab {base} {d} />
          {:else if tab === 'conversation'}
            <ConversationTab {base} {version} />
          {:else if tab === 'timeline'}
            <TimelineTab {d} />
          {:else if tab === 'raw'}
            <RawTab {base} {d} {version} />
          {:else}
            <UsageTab {d} />
          {/if}
        </section>
      {/snippet}
    </StateView>
  </div>
</main>
