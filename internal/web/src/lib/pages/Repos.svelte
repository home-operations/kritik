<script lang="ts">
  import { href } from '../router.svelte';
  import { Paged, live } from '../resource.svelte';
  import { indexTone } from '../format';
  import { repoRoute, accountApi, reindexPath, turnedOnPath } from '../links';
  import { getJSON, sendJSON } from '../api.svelte';
  import { describe, isCode } from '../manage';
  import { isAdmin } from '../session.svelte';
  import { toast } from '../toast.svelte';
  import type { AccountDetail, RegisterResult, Repository, TurnOnRequest } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';
  import ReviewStatusTile from '../components/ReviewStatusTile.svelte';
  import LoadMore from '../components/LoadMore.svelte';
  import RepoTraits from '../components/RepoTraits.svelte';
  import Dialog from '../components/Dialog.svelte';
  import Icon from '../Icon.svelte';
  import { mdiCheck, mdiProgressClock, mdiAlertCircleOutline, mdiSwapHorizontal } from '../icons';
  import type { IndexRunStatus } from '../types';

  let { slug }: { slug: string } = $props();
  let filter = $state('');
  // Which repositories the list shows: by default those kritik can run,
  // neither archived nor forks but the forks turned on; or the forks, or
  // the archived ones.
  let kind = $state<'' | 'forks' | 'archived'>('');
  let filterEl = $state<HTMLInputElement | undefined>(undefined);

  function clearFilter(): void {
    filter = '';
    filterEl?.focus();
  }

  const base = $derived(`${accountApi(slug)}/repos`);
  const paged = new Paged<Repository>(
    (cursor) => `${base}?limit=100${kind ? `&type=${kind}` : ''}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
    (r) => r.id,
  );
  const res = paged.first;

  $effect(() => {
    void paged.load();
  });
  $effect(() => live((e) => e.account === slug && e.kind === 'index_run', () => void paged.load()));

  function visible(): Repository[] {
    const needle = filter.trim().toLowerCase();
    const all = paged.items;
    return needle ? all.filter((r) => r.fullName.toLowerCase().includes(needle)) : all;
  }

  const manage = $derived(isAdmin());
  // Full names picked for a bulk action.
  let selected = $state<string[]>([]);
  let busy = $state(false);
  let confirmReindex = $state(false);

  const isOn = (r: Repository): boolean => r.enabled;
  const indexIcon: Record<IndexRunStatus, string> = {
    running: mdiProgressClock,
    completed: mdiCheck,
    failed: mdiAlertCircleOutline,
    superseded: mdiSwapHorizontal,
  };
  const plural = (n: number): string => (n === 1 ? '1 repository' : `${n} repositories`);

  function pick(fullName: string, on: boolean): void {
    selected = on ? [...selected, fullName] : selected.filter((n) => n !== fullName);
  }

  function pickAll(rows: Repository[], on: boolean): void {
    const names = rows.filter((r) => !r.archived).map((r) => r.fullName);
    selected = on ? [...new Set([...selected, ...names])] : selected.filter((n) => !names.includes(n));
  }

  // setEnabled turns each of names on or off, one request at a time: the
  // dashboard owns a repository's on or off.
  async function setEnabled(names: string[], on: boolean): Promise<void> {
    busy = true;
    const body: TurnOnRequest = { on };
    const failed: string[] = [];
    for (const name of names) {
      try {
        await sendJSON('PUT', turnedOnPath(slug, name), body);
      } catch (err) {
        failed.push(`${name}: ${describe(err)}`);
      }
    }
    busy = false;
    selected = [];
    void paged.load();
    const parts = [`${plural(names.length - failed.length)} turned ${on ? 'on' : 'off'}`];
    if (failed.length) parts.push(`${failed.length} failed (${failed.join('; ')})`);
    toast(parts.join(', '), failed.length ? 'danger' : 'ok');
  }

  // reindex queues a reindex of each selected repository that is on, one
  // request at a time.
  async function reindex(): Promise<void> {
    busy = true;
    const targets = paged.items.filter((r) => selected.includes(r.fullName) && isOn(r)).map((r) => r.fullName);
    let queued = 0;
    let already = 0;
    const failed: string[] = [];
    for (const name of targets) {
      try {
        await sendJSON('POST', reindexPath(slug, name));
        queued++;
      } catch (err) {
        if (isCode(err, 'already_queued')) already++;
        else failed.push(`${name}: ${describe(err)}`);
      }
    }
    busy = false;
    confirmReindex = false;
    selected = [];
    const parts = [`Reindex queued for ${plural(queued)}`];
    if (already) parts.push(`${already} already queued`);
    if (failed.length) parts.push(`${failed.length} failed (${failed.join('; ')})`);
    toast(parts.join(', '), failed.length ? 'danger' : 'ok');
  }

  // resync lists the repositories the account's App reaches again, which
  // records any newly archived, unarchived or added since.
  async function resync(): Promise<void> {
    busy = true;
    try {
      const d = await getJSON<AccountDetail>(accountApi(slug));
      const r = await sendJSON<RegisterResult>('POST', `/api/v1/admin/connections/${encodeURIComponent(d.connection.name)}/repositories`);
      toast(r?.added ? `Resynced from GitHub: ${plural(r.added)} added` : 'Resynced from GitHub');
      void paged.load();
    } catch (err) {
      toast(`Resync failed: ${describe(err)}`, 'danger');
    } finally {
      busy = false;
    }
  }

  // show switches the list to another kind, dropping a selection made in
  // the one it leaves.
  function show(k: typeof kind): void {
    kind = k;
    selected = [];
  }

  const empty = $derived(kind === 'forks' ? 'No forks.' : kind === 'archived' ? 'No archived repositories.' : 'No repositories yet.');

  const selectedOff = $derived(paged.items.filter((r) => selected.includes(r.fullName) && !isOn(r)).length);
</script>

<svelte:head><title>Repositories · {slug} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Repositories</h1></header>
    <div class="toolbar">
      <label class="search-box">
        <span class="sr-only">Filter repositories</span>
        <input type="search" placeholder="Filter by name" bind:value={filter} bind:this={filterEl} />
      </label>
      <label class="select">
        <span class="sr-only">Type</span>
        <select bind:value={() => kind, show} aria-label="Type">
          <option value="">All repositories</option>
          <option value="forks">Forks</option>
          <option value="archived">Archived</option>
        </select>
      </label>
      {#if isAdmin()}
        <button class="btn btn-small" disabled={busy} onclick={resync} title="List the repositories the App reaches again">Resync from GitHub</button>
      {/if}
      {#if manage && selected.length}
        <div class="bulk-actions" role="group" aria-label="Selected repositories">
          <span class="small">{selected.length} selected</span>
          <button class="btn btn-small" disabled={busy} onclick={() => setEnabled(selected, true)}>Turn on</button>
          <button class="btn btn-small" disabled={busy} onclick={() => setEnabled(selected, false)}>Turn off</button>
          <button class="btn btn-small" disabled={busy} onclick={() => (confirmReindex = true)}>Reindex…</button>
          <button class="btn btn-small" disabled={busy} onclick={() => (selected = [])}>Clear</button>
        </div>
      {/if}
    </div>
    {#if kind === 'forks'}
      <p class="small muted">A fork is only reviewed and indexed once turned on here.</p>
    {:else if kind === 'archived'}
      <p class="small muted">An archived repository is never reviewed or indexed. Unarchive it on GitHub, then resync, to turn it on.</p>
    {/if}
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.items.length === 0} {empty}>
      {#snippet children()}
        {@const rows = visible()}
        {#if rows.length === 0}
          <div class="state-msg">
            <span>Nothing matches “{filter}”.</span>
            <button class="btn btn-small" onclick={clearFilter}>Clear filter</button>
          </div>
        {:else}
          <div class="table-wrap table-card">
            <table class="data repo-table">
              <thead>
                <tr>
                  {#if manage}
                    <th scope="col">
                      {#if rows.some((r) => !r.archived)}
                        <input
                          type="checkbox"
                          aria-label="Select every repository shown"
                          checked={rows.every((r) => r.archived || selected.includes(r.fullName))}
                          onchange={(e) => pickAll(rows, e.currentTarget.checked)}
                        />
                      {/if}
                    </th>
                  {/if}
                  <th scope="col">Repository</th>
                  <th scope="col">Enabled</th>
                  <th scope="col">Index</th>
                  <th scope="col">Indexed commit</th>
                  <th scope="col">Last review</th>
                </tr>
              </thead>
              <tbody>
                {#each rows as repo (repo.id)}
                  <tr>
                    {#if manage}
                      <td>
                        {#if !repo.archived}
                          <input
                            type="checkbox"
                            aria-label={`Select ${repo.fullName}`}
                            checked={selected.includes(repo.fullName)}
                            onchange={(e) => pick(repo.fullName, e.currentTarget.checked)}
                          />
                        {/if}
                      </td>
                    {/if}
                    <td class="mono"><a href={href(repoRoute(slug, repo.fullName))}>{repo.fullName}</a> <RepoTraits r={repo} /></td>
                    <td>
                      {#if manage}
                        <label class="toggle" title={repo.archived ? 'Archived on GitHub: unarchive it there first' : undefined}>
                          <input
                            type="checkbox"
                            role="switch"
                            aria-label={`Review and index ${repo.fullName}`}
                            checked={isOn(repo)}
                            disabled={busy || repo.archived}
                            onchange={(e) => setEnabled([repo.fullName], e.currentTarget.checked)}
                          />
                          <span>{isOn(repo) ? 'On' : 'Off'}</span>
                        </label>
                      {:else if repo.enabled}<Pill tone="ok" label="on" />{:else}<Pill label="off" />{/if}
                    </td>
                    <td>
                      {#if repo.index.lastRunStatus}
                        {@const st = repo.index.lastRunStatus}
                        <span class="status tone-{indexTone[st]}">
                          <span class="status-tile"><Icon path={indexIcon[st]} size={12} /></span>
                          <span class="status-word">{st}</span>
                        </span>
                        <Time iso={repo.index.lastRunAt} />
                      {:else}<span class="muted">never</span>{/if}
                    </td>
                    <td class="mono small">{repo.index.activeCommit.slice(0, 7) || '—'}</td>
                    <td>
                      {#if repo.lastReview}
                        <a href={href({ name: 'review', slug, id: repo.lastReview.id })}><ReviewStatusTile status={repo.lastReview.status} /></a>
                        <Time iso={repo.lastReview.createdAt} />
                      {:else}<span class="muted">—</span>{/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
        <LoadMore {paged} />
      {/snippet}
    </StateView>
  </div>
</main>

<Dialog bind:open={confirmReindex} title={`Reindex ${plural(selected.length - selectedOff)}?`}>
  <p>
    Each is indexed again from its default branch, which spends embedder tokens.
    {#if selectedOff}{plural(selectedOff)} that {selectedOff === 1 ? 'is' : 'are'} off {selectedOff === 1 ? 'is' : 'are'} skipped.{/if}
  </p>
  {#snippet footer()}
    <button class="btn" onclick={() => (confirmReindex = false)}>Cancel</button>
    <button class="btn btn-primary" disabled={busy || selected.length === selectedOff} onclick={reindex}>{busy ? 'Queuing…' : 'Reindex'}</button>
  {/snippet}
</Dialog>
