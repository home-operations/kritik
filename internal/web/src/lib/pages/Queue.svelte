<script lang="ts">
  import { getJSON } from '../api.svelte';
  import { href } from '../router.svelte';
  import { Resource, live } from '../resource.svelte';
  import { jobTone, shortSha } from '../format';
  import { pullRoute } from '../links';
  import type { Job } from '../types';
  import StateView from '../components/StateView.svelte';
  import Pill from '../components/Pill.svelte';
  import Time from '../components/Time.svelte';

  let { slug }: { slug: string } = $props();
  const res = new Resource(() => getJSON<Job[]>(`/api/v1/tenants/${encodeURIComponent(slug)}/queue`));

  $effect(() => {
    void res.load();
  });
  $effect(() => live((e) => e.tenant === slug, () => void res.load()));
  // Job state changes (a retry coming due, a worker picking a job up) don't
  // all emit events, so also poll while the page is open.
  $effect(() => {
    const t = setInterval(() => void res.load(), 15_000);
    return () => clearInterval(t);
  });
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head"><h1>Queue</h1></header>
    <StateView {res} retry={() => res.load()} isEmpty={(d) => d.length === 0} empty="The queue is empty.">
      {#snippet children(jobs)}
        <div class="table-wrap">
          <table class="data">
            <thead>
              <tr>
                <th scope="col" class="num">Job</th><th scope="col">Kind</th><th scope="col">State</th><th scope="col" class="num">Attempt</th>
                <th scope="col">Pull</th><th scope="col">Details</th><th scope="col">Scheduled</th><th scope="col">Attempted</th><th scope="col">Last error</th>
              </tr>
            </thead>
            <tbody>
              {#each jobs as j (j.id)}
                <tr>
                  <td class="num mono">{j.id}</td>
                  <td>{j.kind}</td>
                  <td><Pill tone={jobTone[j.state]} label={j.state} /></td>
                  <td class="num">{j.attempt}/{j.maxAttempts}</td>
                  <td class="mono small">
                    {#if j.args.repository && j.args.number}
                      <a href={href(pullRoute(slug, j.args))}>{j.args.repository}#{j.args.number}</a>
                    {:else}{j.args.repository || '—'}{/if}
                  </td>
                  <td class="small">
                    {#if j.args.trigger}<span>{j.args.trigger}</span>{/if}
                    {#if j.args.head}<span class="mono" title={j.args.head}>{shortSha(j.args.head)}</span>{/if}
                    {#if j.args.commentId}<span class="mono">comment {j.args.commentId}</span>{/if}
                  </td>
                  <td><Time iso={j.scheduledAt} /></td>
                  <td><Time iso={j.attemptedAt} /></td>
                  <td class="error-cell">{j.lastError}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/snippet}
    </StateView>
  </div>
</main>
