<script lang="ts">
  // Badges for what GitHub says a repository is: a fork, archived, or both.
  let { r }: { r: { archived: boolean; fork: boolean } } = $props();
</script>

{#if r.fork}<span class="badge" title="A fork of another repository">fork</span>{/if}
{#if r.archived}<span class="badge" title="Archived on GitHub: read-only">archived</span>{/if}
