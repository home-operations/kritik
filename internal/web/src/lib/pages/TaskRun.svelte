<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { duration, pretty, shortSha, splitRepo, taskTone, tokens, usd } from '../format';
  import { pullRoute, repoRoute } from '../links';
  import type { TaskApplied, TaskInline, TaskRunDetail, Transcript } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import Markdown from '../components/Markdown.svelte';
  import Collapsible from '../components/Collapsible.svelte';
  import Conversation from '../components/conversation/Conversation.svelte';

  let { slug, id }: { slug: string; id: string } = $props();
  const base = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}/task-runs/${encodeURIComponent(id)}`);
  const res = new Resource(() => getJSON<TaskRunDetail>(base));
  const transcript = new Resource(() => getJSON<Transcript>(`${base}/transcript`));
  let showTranscript = $state(false);

  $effect(() => {
    void res.load();
  });
  $effect(() => {
    if (showTranscript) void transcript.load();
  });
  $effect(() =>
    live(
      (e) => e.tenant === slug && e.id === id && e.kind === 'task_run',
      () => {
        void res.load();
        if (showTranscript) void transcript.load();
      },
    ),
  );

  // One write the run made or left out, as the actions table lists it.
  interface ActionRow {
    action: string;
    value: string;
    applied: boolean;
    reason: string;
  }

  function inlineValue(c: TaskInline): string {
    return c.endLine > c.line ? `${c.path}:${c.line}-${c.endLine}` : `${c.path}:${c.line}`;
  }

  // The action names match the dropped records' (tasks.Plan's).
  function appliedRows(a: TaskApplied): ActionRow[] {
    const row = (action: string, value: string): ActionRow => ({ action, value, applied: true, reason: '' });
    return [
      ...a.addLabels.map((l) => row('labels.add', l)),
      ...a.removeLabels.map((l) => row('labels.remove', l)),
      ...a.assignees.map((u) => row('assignees', u)),
      ...a.reviewers.map((u) => row('reviewers', u)),
      ...(a.state ? [row('state', a.state)] : []),
      ...a.inline.map((c) => row('inline', inlineValue(c))),
      ...(a.comment ? [row('comment', a.comment)] : []),
    ];
  }

  function actionRows(d: TaskRunDetail): ActionRow[] {
    return [
      ...(d.applied ? appliedRows(d.applied) : []),
      ...d.dropped.map((x) => ({ action: x.action, value: x.value, applied: false, reason: x.reason })),
    ];
  }

  // A field's value as one line: strings bare, anything else as JSON.
  function fieldValue(v: unknown): string {
    return typeof v === 'string' ? v : JSON.stringify(v);
  }
</script>

<main class="page">
  <div class="page-inner">
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        {@const r = d.run}
        {@const n = splitRepo(r.repository)}
        {@const rows = actionRows(d)}
        {@const fields = Object.entries(d.fields)}
        <header class="page-head">
          <p class="crumbs">
            <a href={href({ name: 'taskRuns', slug })}>Task runs</a> /
            <a class="mono" href={href(repoRoute(slug, r.repository))}>{r.repository}</a> /
            <a href={href({ name: 'taskRuns', slug, owner: n.owner, repo: n.repo })}>task runs</a>
          </p>
          <h1>
            <span class="mono">{r.task}</span>
            {#if r.subjectKind === 'pull'}
              <a class="muted" href={href(pullRoute(slug, { repository: r.repository, number: r.subjectNumber }))}>#{r.subjectNumber}</a>
            {:else if r.subjectNumber}
              <span class="muted">#{r.subjectNumber}</span>
            {/if}
          </h1>
          <p class="meta-line">
            <Pill tone={taskTone[r.status]} label={r.status} />
            <span>{r.mode}</span>
            {#if r.trigger}<span class="mono">{r.trigger}</span>{/if}
            {#if r.model}<span class="mono">{r.model}</span>{/if}
            {#if r.configSha}<span class="mono" title={r.configSha}>config {shortSha(r.configSha)}</span>{/if}
            <span>queued <Time iso={r.createdAt} /></span>
            {#if r.durationMs !== null}<span>took {duration(r.durationMs)}</span>{/if}
            {#if d.modelCalls}
              <span title={`${d.tokens.input} in, ${d.tokens.output} out`}>
                {d.modelCalls} model {d.modelCalls === 1 ? 'call' : 'calls'} · {tokens(d.tokens.input + d.tokens.output)} tokens · {usd(d.costUsd)}
              </span>
            {/if}
            {#if r.commentId !== null}<span>comment <span class="mono">{r.commentId}</span></span>{/if}
          </p>
        </header>

        {#if r.reason}<p class="notice" role="note">{r.status === 'skipped' ? 'Skipped' : 'Note'}: {r.reason}</p>{/if}
        {#if r.error}<p class="error-text" role="alert">{r.error}</p>{/if}

        <section class="panel" aria-labelledby="task-actions">
          <header class="panel-head"><h2 id="task-actions">Actions</h2></header>
          {#if rows.length === 0}
            <p class="state-msg">{d.applied ? 'The run wrote nothing.' : 'No actions recorded yet.'}</p>
          {:else}
            <div class="table-wrap">
              <table class="data">
                <thead>
                  <tr><th scope="col">Outcome</th><th scope="col">Action</th><th scope="col">Value</th><th scope="col">Reason</th></tr>
                </thead>
                <tbody>
                  {#each rows as a, i (i)}
                    <tr class={a.applied ? 'action-applied' : 'action-dropped'}>
                      <td><Pill tone={a.applied ? 'ok' : 'warn'} label={a.applied ? 'applied' : 'dropped'} /></td>
                      <td class="mono small">{a.action}</td>
                      <td class="mono small">{a.value || '—'}</td>
                      <td class="small">{a.reason}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </section>

        <div class="grid-2">
          <section class="panel" aria-labelledby="task-fields">
            <header class="panel-head"><h2 id="task-fields">Fields</h2></header>
            {#if fields.length === 0}
              <p class="state-msg">No fields.</p>
            {:else}
              <div class="table-wrap">
                <table class="data">
                  <thead><tr><th scope="col">Field</th><th scope="col">Value</th></tr></thead>
                  <tbody>
                    {#each fields as [k, v] (k)}
                      <tr><td class="mono small">{k}</td><td class="mono small">{fieldValue(v)}</td></tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            {/if}
          </section>
          <section class="panel" aria-labelledby="task-event">
            <header class="panel-head"><h2 id="task-event">Event</h2></header>
            {#if !d.event}
              <p class="state-msg">The event is past its retention.</p>
            {:else}
              {@const e = d.event}
              <dl class="deflist">
                <dt>Event</dt><dd class="mono">{e.rawEvent}{e.action ? `.${e.action}` : ''}</dd>
                <dt>As</dt><dd class="mono">{e.event || '—'}</dd>
                <dt>Sender</dt><dd class="mono">{e.sender || '—'}</dd>
                <dt>Forge</dt><dd>{e.forge}</dd>
                <dt>Delivery</dt><dd class="mono small">{e.delivery || '—'}</dd>
                <dt>Received</dt><dd><Time iso={e.receivedAt} /></dd>
              </dl>
            {/if}
          </section>
        </div>

        {#if d.proposed}
          <section class="panel" aria-labelledby="task-answer">
            <header class="panel-head"><h2 id="task-answer">Model answer</h2></header>
            {#if d.proposed.summary}<Markdown text={d.proposed.summary} />{/if}
            {#if d.proposed.comment}
              <h3 class="small muted">Comment</h3>
              <Markdown text={d.proposed.comment} />
            {/if}
            <Collapsible title="Proposed, as the model answered">
              <pre class="mono small">{pretty(d.proposed)}</pre>
            </Collapsible>
          </section>
        {/if}

        <section class="panel" aria-labelledby="task-transcript">
          <header class="panel-head"><h2 id="task-transcript">Model conversation</h2></header>
          <Collapsible title="Transcript" bind:open={showTranscript}>
            <StateView res={transcript} retry={() => transcript.load()} isEmpty={(t) => t.turns.length === 0} empty="No model calls recorded for this run.">
              {#snippet children(t)}
                <Conversation transcript={t} />
              {/snippet}
            </StateView>
          </Collapsible>
        </section>
      {/snippet}
    </StateView>
  </div>
</main>
