<script lang="ts">
  // Stands in for every page a later task will build. Renders the route name
  // and its params so a deep link is at least verifiable during development.
  import type { Route } from './router.svelte';

  interface Props {
    route: Route;
  }
  let { route }: Props = $props();

  const titles: Record<Route['name'], string> = {
    overview: 'Overview',
    signin: 'Sign in',
    operator: 'Operator console',
    tenant: 'Tenant overview',
    repos: 'Repos',
    repo: 'Repo',
    pulls: 'Pull requests',
    pull: 'Pull request',
    review: 'Review',
    queue: 'Queue',
    usage: 'Usage',
    followups: 'Follow-ups',
    admin: 'Admin',
  };
</script>

<main class="placeholder">
  <h1>{titles[route.name]}</h1>
  <p class="placeholder-note">Not implemented yet.</p>
  <p class="mono placeholder-route">{JSON.stringify(route)}</p>
</main>
