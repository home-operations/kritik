<script lang="ts">
  // Shown instead of a repository or pull request when several
  // installations hold the same owner/repo and the route named none: one
  // link per installation, each to the same page for that installation.
  import { href } from '../router.svelte';
  import type { Route } from '../routes';

  let { name, installations, route }: { name: string; installations: string[]; route: (installation: string) => Route } =
    $props();
</script>

<section class="panel" role="alert" aria-labelledby="installation-choice">
  <header class="panel-head"><h2 id="installation-choice">Which installation?</h2></header>
  <p class="state-msg">Several installations hold <span class="mono">{name}</span>. Pick the one you mean:</p>
  <ul class="rows">
    {#each installations as installation (installation)}
      <li class="row"><a class="row-link mono" href={href(route(installation))}>{installation}</a></li>
    {/each}
  </ul>
</section>
