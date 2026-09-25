<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { duration, indexTone, shortSha, bytes } from '../format';
  import type { Page, Pull, RepoDetail } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import PullRows from '../components/PullRows.svelte';

  let { slug, owner, repo }: { slug: string; owner: string; repo: string } = $props();
  const fullName = $derived(`${owner}/${repo}`);
  const tenant = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}`);

  const res = new Resource(() => getJSON<RepoDetail>(`${tenant}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`));
  const pulls = new Resource(() => getJSON<Page<Pull>>(`${tenant}/pulls?state=all&limit=50&repo=${encodeURIComponent(fullName)}`));

  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void pulls.load();
  });
  $effect(() =>
    live(
      (e) => e.tenant === slug && e.kind !== 'model_call',
      () => {
        void res.load();
        void pulls.load();
      },
    ),
  );

  const list = (xs: string[]) => (xs.length ? xs.join(', ') : '—');
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <p class="crumbs"><a href={href({ name: 'repos', slug })}>Repositories</a> /</p>
      <h1 class="mono">{fullName}</h1>
    </header>
    <StateView {res} retry={() => res.load()}>
      {#snippet children(d)}
        {@const s = d.settings}
        <div class="grid-2">
          <section class="panel" aria-labelledby="repo-settings">
            <header class="panel-head"><h2 id="repo-settings">Effective settings</h2></header>
            <dl class="deflist">
              <dt>Enabled</dt><dd>{s.enabled ? 'yes' : 'no'} <span class="muted small">({d.managedBy})</span></dd>
              <dt>Installation</dt><dd class="mono">{d.installation}</dd>
              <dt>Default branch</dt><dd class="mono">{d.defaultBranch}</dd>
              <dt>Mode</dt><dd>{s.mode}</dd>
              <dt>Review model</dt><dd class="mono">{s.models.review || '—'}</dd>
              <dt>Fallback model</dt><dd class="mono">{s.models.fallback || '—'}</dd>
              <dt>Filter</dt><dd class="mono">{s.filter || '—'}</dd>
              <dt>Forks</dt><dd>{s.forks ? 'reviewed' : 'skipped'}</dd>
              <dt>Ignore</dt><dd class="mono">{list(s.ignore)}</dd>
              <dt>Settle</dt><dd>{duration(s.settleSeconds * 1000) || '0s'}</dd>
              <dt>Max delta files</dt><dd>{s.maxDeltaFiles}</dd>
              <dt>Instructions</dt><dd class="mono">{list(s.review.instructions)}</dd>
              <dt>Require suggested fix</dt><dd>{s.review.requireSuggestedFix ? 'yes' : 'no'}</dd>
              <dt>Concurrency</dt><dd>{s.limits.concurrency || 'unlimited'}</dd>
              <dt>Reviews / day</dt><dd>{s.limits.reviewsPerDay || 'unlimited'}</dd>
              <dt>Tokens / month</dt><dd>{s.limits.tokensPerMonth || 'unlimited'}</dd>
            </dl>
          </section>
          <section class="panel" aria-labelledby="repo-agent">
            <header class="panel-head"><h2 id="repo-agent">Agent limits</h2></header>
            <dl class="deflist">
              <dt>Max steps</dt><dd>{s.agent.maxSteps}</dd>
              <dt>Max tool output</dt><dd>{bytes(s.agent.maxToolOutputBytes)}</dd>
              <dt>Max tokens</dt><dd>{s.agent.maxTokens}</dd>
              <dt>Timeout</dt><dd>{duration(s.agent.timeoutSeconds * 1000)}</dd>
              <dt>Commands</dt><dd class="mono">{list(s.agent.commands)}</dd>
              <dt>Command timeout</dt><dd>{duration(s.agent.commandTimeoutSeconds * 1000)}</dd>
            </dl>
          </section>
        </div>

        <section class="panel" aria-labelledby="repo-index">
          <header class="panel-head"><h2 id="repo-index">Index runs</h2></header>
          {#if d.indexRuns.length === 0}
            <p class="state-msg">Never indexed.</p>
          {:else}
            <div class="table-wrap">
              <table class="data">
                <thead>
                  <tr>
                    <th scope="col">Status</th><th scope="col">Mode</th><th scope="col">Commit</th><th scope="col">Trigger</th>
                    <th scope="col" class="num">Chunks</th><th scope="col">Embed model</th><th scope="col">Started</th>
                    <th scope="col">Took</th><th scope="col">Error</th>
                  </tr>
                </thead>
                <tbody>
                  {#each d.indexRuns as run (run.id)}
                    <tr>
                      <td><Pill tone={indexTone[run.status]} label={run.status} /></td>
                      <td>{run.mode}</td>
                      <td class="mono small" title={run.commitSha}>{shortSha(run.commitSha)}{run.baseSha ? ` ← ${shortSha(run.baseSha)}` : ''}</td>
                      <td>{run.trigger}</td>
                      <td class="num">{run.chunkCount}</td>
                      <td class="mono small">{run.embedModel}</td>
                      <td><Time iso={run.createdAt} /></td>
                      <td>{run.finishedAt ? duration(Date.parse(run.finishedAt) - Date.parse(run.createdAt)) : '…'}</td>
                      <td class="error-cell">{run.error}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </section>
      {/snippet}
    </StateView>

    <section class="panel" aria-labelledby="repo-pulls">
      <header class="panel-head"><h2 id="repo-pulls">Pull requests</h2></header>
      <StateView res={pulls} retry={() => pulls.load()} isEmpty={(p) => p.items.length === 0} empty="No pull requests seen yet.">
        {#snippet children(p)}
          <PullRows {slug} items={p.items} />
        {/snippet}
      </StateView>
    </section>
  </div>
</main>
