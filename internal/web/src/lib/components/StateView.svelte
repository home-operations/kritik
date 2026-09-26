<script lang="ts" generics="T">
  // The loading / error / empty / loaded switch every page shares. Keeps the
  // previous data on screen during a background refetch so a live update
  // doesn't flash a spinner.
  import type { Snippet } from 'svelte';
  import type { Resource } from '../resource.svelte';
  import { errorMessage } from '../resource.svelte';
  import Spinner from '../Spinner.svelte';

  interface Props {
    res: Resource<T>;
    children: Snippet<[T]>;
    isEmpty?: (data: T) => boolean;
    empty?: string;
    retry?: () => void;
  }
  let { res, children, isEmpty, empty = 'Nothing here yet.', retry }: Props = $props();
</script>

{#if res.data !== undefined}
  {#if res.error}
    <p class="state-msg state-error" role="alert">Refresh failed: {errorMessage(res.error)}</p>
  {/if}
  {#if isEmpty?.(res.data)}
    <p class="state-msg">{empty}</p>
  {:else}
    {@render children(res.data)}
  {/if}
{:else if res.error}
  <div class="state-msg state-error" role="alert">
    <span>{errorMessage(res.error)}</span>
    {#if retry}<button class="btn" onclick={retry}>Retry</button>{/if}
  </div>
{:else}
  <p class="state-msg" aria-live="polite"><Spinner size={16} /> Loading…</p>
{/if}
