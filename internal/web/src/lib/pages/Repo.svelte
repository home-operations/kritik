<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { duration, indexTone, shortSha, bytes } from '../format';
  import type { ConfigSource, Page, Pull, RepoDetail, RepoSettings } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import PullTable from '../components/PullTable.svelte';
  import ActionButton from '../components/ActionButton.svelte';
  import RepoTraits from '../components/RepoTraits.svelte';
  import { reindexPath, accountApi } from '../links';
  import { isAdmin } from '../session.svelte';

  let { slug, owner, repo }: { slug: string; owner: string; repo: string } = $props();
  const fullName = $derived(`${owner}/${repo}`);
  const account = $derived(`${accountApi(slug)}`);

  const res = new Resource(() =>
    getJSON<RepoDetail>(`${account}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`),
  );
  const pulls = new Resource(() =>
    getJSON<Page<Pull>>(
      `${account}/pulls?state=all&limit=50&repo=${encodeURIComponent(fullName)}`,
    ),
  );

  $effect(() => {
    void res.load();
  });
  $effect(() => {
    void pulls.load();
  });
  $effect(() =>
    live(
      (e) => e.account === slug && e.kind !== 'model_call',
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
    { label: 'Thoroughness', key: 'review.thoroughness', value: (s) => s.review.thoroughness },
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
    defaults: 'defaults',
    account: 'account',
    repository: '.kritik.yaml',
  };

  // settingFilter narrows the settings shown to those whose label, key or
  // value holds it.
  let settingFilter = $state('');
  function matches(...texts: string[]): boolean {
    const needle = settingFilter.trim().toLowerCase();
    return !needle || texts.some((t) => t.toLowerCase().includes(needle));
  }
  function shown(d: RepoDetail, rows: Row[]): Row[] {
    const eff = d.repoConfig?.settings ?? d.settings;
    return rows.filter((r) => matches(r.label, r.key, r.value(eff)));
  }

  // The bounds a repository's .kritik.yaml chooses within, each "own" when
  // the admin set none.
  function bounds(s: RepoSettings): { label: string; value: string }[] {
    const a = s.allow;
    const most = (n: number | null, own: string) => (n === null ? `at most the admin's ${own}` : `at most ${n}`);
    return [
      { label: 'Modes', value: a.modes ? list(a.modes) : `the admin's own (${s.mode})` },
      { label: 'Models', value: a.models ? list(a.models) : "the admin's own" },
      { label: 'Commands', value: a.commands ? list(a.commands) : `some of the admin's (${list(s.agent.commands)})` },
      { label: 'Max steps', value: most(a.agent.maxSteps, String(s.agent.maxSteps)) },
      { label: 'Max tokens', value: most(a.agent.maxTokens, String(s.agent.maxTokens)) },
      {
        label: 'Settle',
        value: a.settleSeconds === null ? `at most the admin's ${duration(s.settleSeconds * 1000) || '0s'}` : `at most ${duration(a.settleSeconds * 1000) || '0s'}`,
      },
    ];
  }
</script>

<svelte:head><title>{fullName} · kritik</title></svelte:head>

{#snippet setting(d: RepoDetail, r: Row)}
  {@const eff = d.repoConfig?.settings ?? d.settings}
  {@const value = r.value(eff)}
  {@const own = r.value(d.settings)}
  <dt>{r.label}</dt>
  <dd>
    <span class:mono={r.mono}>{value}</span>
    {#if value !== own}
      <span class="muted small">({sourceLabel.repository}; the admin's is <span class:mono={r.mono}>{own}</span>)</span>
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
      {#if res.data?.fork || res.data?.archived}<p class="meta-line"><RepoTraits r={res.data} /></p>{/if}
      {#if isAdmin()}
        <div class="page-actions">
          <ActionButton
            label="Reindex"
            title="Reindex the repository?"
            body={`Rebuild the code index of ${fullName} from scratch at its default branch.`}
            path={reindexPath(slug, fullName)}
            done="Reindex queued"
            ondone={() => res.load()}
          />
        </div>
      {/if}
    </header>
    <StateView {res} retry={() => res.load()}>
        {#snippet children(d)}
          {@const s = d.settings}
          {@const rc = d.repoConfig}
          {@const enabled = d.enabled && (rc?.settings.enabled ?? true) ? 'yes' : 'no'}
          {@const why = d.archived ? 'archived on GitHub' : d.fork && !d.enabled ? 'a fork not turned on' : d.managedBy}
          {@const rows = shown(d, settingRows)}
          {@const agent = shown(d, agentRows)}
          <div class="grid-2">
            <section class="panel" aria-labelledby="repo-settings">
              <header class="panel-head">
                <h2 id="repo-settings">Effective settings</h2>
                <input class="settings-filter" type="search" aria-label="Filter settings" placeholder="Filter settings" bind:value={settingFilter} />
              </header>
              <dl class="deflist">
                {#if matches('Enabled', 'enabled', enabled)}
                  <dt>Enabled</dt><dd>{enabled} <span class="muted small">({why})</span></dd>
                {/if}
                {#if matches('Default branch', d.defaultBranch)}<dt>Default branch</dt><dd class="mono">{d.defaultBranch}</dd>{/if}
                {#each rows as r (r.label)}
                  {@render setting(d, r)}
                {/each}
              </dl>
              {#if rows.length === 0 && agent.length === 0}<p class="state-msg">No setting matches “{settingFilter.trim()}”.</p>{/if}
            </section>
            <section class="panel" aria-labelledby="repo-agent">
              <header class="panel-head"><h2 id="repo-agent">Agent limits</h2></header>
              <dl class="deflist">
                {#each agent as r (r.label)}
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
                    <dt>Filter</dt><dd class="mono">{rc.filter || '—'} <span class="muted small">(ANDed with the admin's)</span></dd>
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
                  <p class="small">Values outside the admin's bounds, where the admin's apply instead:</p>
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
            <PullTable {slug} items={p.items} />
          {/snippet}
        </StateView>
      </section>
  </div>
</main>
