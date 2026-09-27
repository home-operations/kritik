<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Paged, Resource, live } from '../resource.svelte';
  import { duration, taskTone } from '../format';
  import { pullRoute, repoRoute } from '../links';
  import type { Page, Repository, TaskRun, TaskRunStatus } from '../types';
  import StateView from '../components/StateView.svelte';
  import LoadMore from '../components/LoadMore.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';

  let { slug, owner, repo, installation }: { slug: string; owner?: string; repo?: string; installation?: string } = $props();

  const STATUSES: readonly TaskRunStatus[] = ['queued', 'running', 'succeeded', 'failed', 'skipped'];

  // A repository's page fixes the repository; the tenant's lets it be chosen.
  const fixedRepo = $derived(owner !== undefined && repo !== undefined ? `${owner}/${repo}` : '');
  let repoName = $state('');
  let status = $state<TaskRunStatus | ''>('');
  let taskInput = $state('');
  let task = $state('');

  const tenant = $derived(`/api/v1/tenants/${encodeURIComponent(slug)}`);

  function query(after?: string): string {
    const p = new URLSearchParams({ limit: '50' });
    const r = fixedRepo || repoName;
    if (r) p.set('repo', r);
    if (fixedRepo && installation) p.set('installation', installation);
    if (task) p.set('task', task);
    if (status) p.set('status', status);
    if (after) p.set('cursor', after);
    return `${tenant}/task-runs?${p}`;
  }

  const paged = new Paged<TaskRun>(query, (r) => r.id);
  const res = paged.first;
  const repos = new Resource(() => getJSON<Page<Repository>>(`${tenant}/repos?limit=100`));

  $effect(() => {
    void paged.load();
  });
  $effect(() => {
    if (!fixedRepo) void repos.load();
  });
  $effect(() => live((e) => e.tenant === slug && e.kind === 'task_run', () => void paged.load()));
  $effect(() => {
    const v = taskInput.trim();
    const t = setTimeout(() => (task = v), 250);
    return () => clearTimeout(t);
  });

  const items = $derived(paged.items);
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      {#if fixedRepo}
        <p class="crumbs">
          <a href={href({ name: 'repos', slug })}>Repositories</a> /
          <a class="mono" href={href(repoRoute(slug, fixedRepo, installation))}>{fixedRepo}</a> /
        </p>
      {/if}
      <h1>Task runs</h1>
    </header>
    <div class="toolbar" role="search">
      <label class="search-box">
        <span class="sr-only">Task</span>
        <input type="search" placeholder="Task name" bind:value={taskInput} />
      </label>
      {#if !fixedRepo}
        <label class="select">
          <span class="sr-only">Repository</span>
          <select bind:value={repoName} aria-label="Repository">
            <option value="">All repositories</option>
            {#each repos.data?.items ?? [] as r (r.id)}
              <option value={r.fullName}>{r.fullName}</option>
            {/each}
          </select>
        </label>
      {/if}
      <label class="select">
        <span class="sr-only">Status</span>
        <select bind:value={status} aria-label="Status">
          <option value="">Any status</option>
          {#each STATUSES as s (s)}<option value={s}>{s}</option>{/each}
        </select>
      </label>
    </div>
    <StateView {res} retry={() => res.load()} isEmpty={() => items.length === 0} empty="No task runs match.">
      {#snippet children()}
        <div class="table-wrap">
          <table class="data task-runs">
            <thead>
              <tr>
                <th scope="col">Status</th><th scope="col">Task</th>
                {#if !fixedRepo}<th scope="col">Repository</th>{/if}
                <th scope="col">Subject</th><th scope="col">Trigger</th><th scope="col">Mode</th>
                <th scope="col">Outcome</th><th scope="col">Queued</th><th scope="col">Took</th>
              </tr>
            </thead>
            <tbody>
              {#each items as r (r.id)}
                <tr>
                  <td><Pill tone={taskTone[r.status]} label={r.status} /></td>
                  <td class="mono"><a href={href({ name: 'taskRun', slug, id: r.id })}>{r.task}</a></td>
                  {#if !fixedRepo}<td class="mono small">{r.repository}</td>{/if}
                  <td class="mono small">
                    {#if r.subjectKind === 'pull'}
                      <a href={href(pullRoute(slug, { repository: r.repository, number: r.subjectNumber }, fixedRepo ? installation : undefined))}>#{r.subjectNumber}</a>
                    {:else if r.subjectNumber}
                      #{r.subjectNumber}
                    {:else}
                      —
                    {/if}
                  </td>
                  <td class="mono small">{r.trigger || '—'}</td>
                  <td>{r.mode}</td>
                  <td class="small">
                    {#if r.error}<span class="error-text">{r.error}</span>
                    {:else if r.reason}<span class="muted">{r.reason}</span>
                    {:else if r.droppedCount}<span>{r.droppedCount} dropped</span>
                    {/if}
                  </td>
                  <td><Time iso={r.createdAt} /></td>
                  <td>{r.durationMs !== null ? duration(r.durationMs) : '…'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        <LoadMore {paged} />
      {/snippet}
    </StateView>
  </div>
</main>
