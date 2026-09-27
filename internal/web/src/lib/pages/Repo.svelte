<script lang="ts">
  import { ambiguousInstallations, getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { duration, indexTone, shortSha, bytes } from '../format';
  import type { ConfigSource, Page, Pull, RepoDetail, RepoSettings } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import PullRows from '../components/PullRows.svelte';
  import ActionButton from '../components/ActionButton.svelte';
  import InstallationChoice from '../components/InstallationChoice.svelte';
  import { installationQuery, reindexPath, repoRoute } from '../links';
  import { canAdmin } from '../session.svelte';

  let { slug, owner, repo, installation }: { slug: string; owner: string; repo: string; installation?: string } = $props();
  const fullName = $derived(`${owner}/${repo}`);
  const tenant = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}`);

  const res = new Resource(() =>
    getJSON<RepoDetail>(`${tenant}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}${installationQuery(installation)}`),
  );
  const pulls = new Resource(() =>
    getJSON<Page<Pull>>(
      `${tenant}/pulls?state=all&limit=50&repo=${encodeURIComponent(fullName)}${installation ? `&installation=${encodeURIComponent(installation)}` : ''}`,
    ),
  );
  const choices = $derived(ambiguousInstallations(res.error));

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
  const yes = (b: boolean) => (b ? 'yes' : 'no');
  const unlimited = (n: number) => (n ? String(n) : 'unlimited');

  // One setting as the page shows it: its label, the policy key its source
  // is reported under, and how to show its value.
  interface Row {
    label: string;
    key: string;
    value: (s: RepoSettings) => string;
    mono?: boolean;
  }
  const settingRows: Row[] = [
    { label: 'Mode', key: 'mode', value: (s) => s.mode },
    { label: 'Review model', key: 'models.review', value: (s) => s.models.review || '—', mono: true },
    { label: 'Fallback model', key: 'models.fallback', value: (s) => s.models.fallback || '—', mono: true },
    { label: 'Filter', key: 'filter', value: (s) => s.filter || '—', mono: true },
    { label: 'Forks', key: 'forks', value: (s) => (s.forks ? 'reviewed' : 'skipped') },
    { label: 'Ignore', key: 'ignore', value: (s) => list(s.ignore), mono: true },
    { label: 'Settle', key: 'settle', value: (s) => duration(s.settleSeconds * 1000) || '0s' },
    { label: 'Max delta files', key: 'incremental.maxDeltaFiles', value: (s) => String(s.maxDeltaFiles) },
    { label: 'Instructions', key: 'review.instructions', value: (s) => list(s.review.instructions), mono: true },
    { label: 'Context files', key: 'review.context', value: (s) => list(s.review.context.map((c) => c.path)), mono: true },
    { label: 'Require suggested fix', key: 'review.requireSuggestedFix', value: (s) => yes(s.review.requireSuggestedFix) },
    { label: 'Inline comments', key: 'review.inlineComments', value: (s) => yes(s.review.inlineComments) },
    { label: 'Inline severity floor', key: 'review.minSeverity', value: (s) => s.review.minSeverity || 'every finding' },
    { label: 'Concurrency', key: 'limits', value: (s) => unlimited(s.limits.concurrency) },
    { label: 'Reviews / day', key: 'limits', value: (s) => unlimited(s.limits.reviewsPerDay) },
    { label: 'Tokens / month', key: 'limits', value: (s) => unlimited(s.limits.tokensPerMonth) },
  ];
  const agentRows: Row[] = [
    { label: 'Max steps', key: 'agent.maxSteps', value: (s) => String(s.agent.maxSteps) },
    { label: 'Max tool output', key: 'agent.maxToolOutputBytes', value: (s) => bytes(s.agent.maxToolOutputBytes) },
    { label: 'Max tokens', key: 'agent.maxTokens', value: (s) => String(s.agent.maxTokens) },
    { label: 'Timeout', key: 'agent.timeout', value: (s) => duration(s.agent.timeoutSeconds * 1000) },
    { label: 'Commands', key: 'agent.commands', value: (s) => list(s.agent.commands), mono: true },
    { label: 'Command timeout', key: 'agent.commandTimeout', value: (s) => duration(s.agent.commandTimeoutSeconds * 1000) },
  ];
  const sourceLabel: Record<ConfigSource, string> = {
    default: 'default',
    env: 'environment',
    file: 'config file',
    dashboard: 'dashboard',
    repository: '.kritik.yaml',
  };

  // The bounds a repository's .kritik.yaml chooses within, each "own" when
  // the operator set none.
  function bounds(s: RepoSettings): { label: string; value: string }[] {
    const a = s.allow;
    const most = (n: number | null, own: string) => (n === null ? `at most the operator's ${own}` : `at most ${n}`);
    return [
      { label: 'Modes', value: a.modes ? list(a.modes) : `the operator's own (${s.mode})` },
      { label: 'Models', value: a.models ? list(a.models) : "the operator's own" },
      { label: 'Commands', value: a.commands ? list(a.commands) : `some of the operator's (${list(s.agent.commands)})` },
      { label: 'Max steps', value: most(a.agent.maxSteps, String(s.agent.maxSteps)) },
      { label: 'Max tokens', value: most(a.agent.maxTokens, String(s.agent.maxTokens)) },
      {
        label: 'Settle',
        value: a.settleSeconds === null ? `at most the operator's ${duration(s.settleSeconds * 1000) || '0s'}` : `at most ${duration(a.settleSeconds * 1000) || '0s'}`,
      },
    ];
  }
</script>

{#snippet setting(d: RepoDetail, r: Row)}
  {@const eff = d.repoConfig?.settings ?? d.settings}
  {@const value = r.value(eff)}
  {@const own = r.value(d.settings)}
  <dt>{r.label}</dt>
  <dd>
    <span class:mono={r.mono}>{value}</span>
    {#if value !== own}
      <span class="muted small">({sourceLabel.repository}; the operator's is <span class:mono={r.mono}>{own}</span>)</span>
    {:else}
      <span class="muted small">({sourceLabel[d.sources[r.key] ?? 'default']})</span>
    {/if}
  </dd>
{/snippet}

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <p class="crumbs"><a href={href({ name: 'repos', slug })}>Repositories</a> /</p>
      <h1 class="mono">{fullName}</h1>
      <p class="meta-line"><a href={href({ name: 'taskRuns', slug, owner, repo, ...(installation ? { installation } : {}) })}>Task runs</a></p>
      {#if canAdmin(slug) && !choices}
        <div class="page-actions">
          <ActionButton
            label="Reindex"
            title="Reindex the repository?"
            body={`Rebuild the code index of ${fullName} from scratch at its default branch.`}
            path={reindexPath(slug, fullName, installation)}
            done="Reindex queued"
            ondone={() => res.load()}
          />
        </div>
      {/if}
    </header>
    {#if choices}
      <InstallationChoice name={fullName} installations={choices} route={(i) => repoRoute(slug, fullName, i)} />
    {:else}
      <StateView {res} retry={() => res.load()}>
        {#snippet children(d)}
          {@const s = d.settings}
          {@const rc = d.repoConfig}
          <div class="grid-2">
            <section class="panel" aria-labelledby="repo-settings">
              <header class="panel-head"><h2 id="repo-settings">Effective settings</h2></header>
              <dl class="deflist">
                <dt>Enabled</dt><dd>{s.enabled && (rc?.settings.enabled ?? true) ? 'yes' : 'no'} <span class="muted small">({d.managedBy})</span></dd>
                <dt>Installation</dt><dd class="mono">{d.installation}</dd>
                <dt>Default branch</dt><dd class="mono">{d.defaultBranch}</dd>
                {#each settingRows as r (r.label)}
                  {@render setting(d, r)}
                {/each}
              </dl>
            </section>
            <section class="panel" aria-labelledby="repo-agent">
              <header class="panel-head"><h2 id="repo-agent">Agent limits</h2></header>
              <dl class="deflist">
                {#each agentRows as r (r.label)}
                  {@render setting(d, r)}
                {/each}
              </dl>
            </section>
          </div>

          <div class="grid-2">
            <section class="panel" aria-labelledby="repo-file">
              <header class="panel-head"><h2 id="repo-file" class="mono">.kritik.yaml</h2></header>
              {#if !rc}
                <p class="state-msg">No review has read it yet.</p>
              {:else}
                <dl class="deflist">
                  <dt>Read at</dt>
                  <dd>
                    <span class="mono" title={rc.commit}>{shortSha(rc.commit)}</span>
                    <span class="muted small">(merge base of <a href={href({ name: 'review', slug, id: rc.reviewId })}>the last review</a>)</span>
                  </dd>
                  {#if rc.found}
                    <dt>Filter</dt><dd class="mono">{rc.filter || '—'} <span class="muted small">(ANDed with the operator's)</span></dd>
                    <dt>Skip when only these change</dt><dd class="mono">{list(rc.skipPaths)}</dd>
                  {/if}
                </dl>
                {#if !rc.found}
                  <p class="state-msg">There was no .kritik.yaml at that commit.</p>
                {/if}
                {#if rc.ignored}
                  <p class="notice" role="note">Ignored as a whole: {rc.ignored}</p>
                {/if}
                {#if rc.dropped.length}
                  <p class="small">Values outside the operator's bounds, where the operator's apply instead:</p>
                  <ul class="small">
                    {#each rc.dropped as note (note)}<li>{note}</li>{/each}
                  </ul>
                {/if}
              {/if}
            </section>
            <section class="panel" aria-labelledby="repo-bounds">
              <header class="panel-head"><h2 id="repo-bounds">What .kritik.yaml may choose</h2></header>
              <dl class="deflist">
                {#each bounds(s) as b (b.label)}
                  <dt>{b.label}</dt><dd class="mono">{b.value}</dd>
                {/each}
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
            <PullRows {slug} items={p.items} {installation} />
          {/snippet}
        </StateView>
      </section>
    {/if}
  </div>
</main>
