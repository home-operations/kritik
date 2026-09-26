<script lang="ts">
  // Maps the active route to its page. Keyed so switching tenant or record
  // starts the page fresh (filters, cursors), while switching review tabs
  // keeps the review page mounted.
  import type { Route } from '../routes';
  import Placeholder from '../Placeholder.svelte';
  import Overview from './Overview.svelte';
  import Operator from './Operator.svelte';
  import TenantOverview from './TenantOverview.svelte';
  import Repos from './Repos.svelte';
  import Repo from './Repo.svelte';
  import Pulls from './Pulls.svelte';
  import Pull from './Pull.svelte';
  import Review from './review/Review.svelte';
  import Queue from './Queue.svelte';
  import Usage from './Usage.svelte';
  import Followups from './Followups.svelte';
  import Admin from './admin/Admin.svelte';

  let { route }: { route: Route } = $props();

  function keyOf(r: Route): string {
    if (r.name === 'review') return JSON.stringify({ n: r.name, s: r.slug, id: r.id });
    return JSON.stringify(r);
  }
  const key = $derived(keyOf(route));
</script>

<!-- data-route exposes the parsed route to the router tests. -->
<div class="route-host" data-route={JSON.stringify(route)}>
{#key key}
  {#if route.name === 'overview'}
    <Overview />
  {:else if route.name === 'operator'}
    <Operator />
  {:else if route.name === 'tenant'}
    <TenantOverview slug={route.slug} />
  {:else if route.name === 'repos'}
    <Repos slug={route.slug} />
  {:else if route.name === 'repo'}
    <Repo slug={route.slug} owner={route.owner} repo={route.repo} installation={route.installation} />
  {:else if route.name === 'pulls'}
    <Pulls slug={route.slug} />
  {:else if route.name === 'pull'}
    <Pull slug={route.slug} owner={route.owner} repo={route.repo} number={route.number} installation={route.installation} />
  {:else if route.name === 'review'}
    <Review slug={route.slug} id={route.id} tab={route.tab} />
  {:else if route.name === 'queue'}
    <Queue slug={route.slug} />
  {:else if route.name === 'usage'}
    <Usage slug={route.slug} />
  {:else if route.name === 'followups'}
    <Followups slug={route.slug} />
  {:else if route.name === 'admin'}
    <Admin slug={route.slug} section={route.section} />
  {:else}
    <Placeholder {route} />
  {/if}
{/key}
</div>
