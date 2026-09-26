<script lang="ts">
  import type { Followup } from '../types';
  import { followupTone } from '../format';
  import { href } from '../router.svelte';
  import { pullRoute } from '../links';
  import Pill from './Pill.svelte';
  import Time from './Time.svelte';
  import FollowupTranscript from './FollowupTranscript.svelte';

  let { slug, f, showPull = false }: { slug: string; f: Followup; showPull?: boolean } = $props();
  let open = $state(false);
  const id = $derived(`fu-${f.id}`);
</script>

<li class="followup">
  <div class="followup-head">
    <Pill tone={followupTone[f.status]} label={f.status} title={f.reason || undefined} />
    <span><strong>{f.author}</strong> asked</span>
    {#if f.inline}<span class="mono small">{f.path}:{f.line}</span>{:else}<span class="small muted">on the conversation</span>{/if}
    {#if showPull}
      <a class="mono small" href={href(pullRoute(slug, f))}>{f.repository}#{f.number}</a>
    {/if}
    {#if f.model}<span class="mono small muted">{f.model}</span>{/if}
    <Time iso={f.createdAt} />
    <span class="spacer"></span>
    <button class="btn btn-small" aria-expanded={open} aria-controls={open ? id : undefined} onclick={() => (open = !open)}>
      {open ? 'Hide transcript' : 'Transcript'}
    </button>
  </div>
  {#if f.reason}<p class="small muted followup-reason">{f.reason}</p>{/if}
  <p class="small muted">
    Comment <span class="mono">{f.commentId}</span>{#if f.replyCommentId !== null}, answered in <span class="mono">{f.replyCommentId}</span>{/if}
  </p>
  {#if open}
    <div {id} class="followup-transcript"><FollowupTranscript {slug} commentId={f.commentId} /></div>
  {/if}
</li>
