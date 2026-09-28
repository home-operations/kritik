<script lang="ts">
  // Shown instead of a repository or pull request when several
  // connections hold the same owner/repo and the route named none: one
  // link per connection, each to the same page for that connection.
  import { href } from '../router.svelte';
  import type { Route } from '../routes';

  let { name, connections, route }: { name: string; connections: string[]; route: (connection: string) => Route } =
    $props();
</script>

<section class="panel" role="alert" aria-labelledby="connection-choice">
  <header class="panel-head"><h2 id="connection-choice">Which connection?</h2></header>
  <p class="state-msg">Several connections hold <span class="mono">{name}</span>. Pick the one you mean:</p>
  <ul class="rows">
    {#each connections as connection (connection)}
      <li class="row"><a class="row-link mono" href={href(route(connection))}>{connection}</a></li>
    {/each}
  </ul>
</section>
