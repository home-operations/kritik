<script lang="ts">
  // A disclosure: a real button with aria-expanded controlling a region, so
  // it is keyboard- and screen-reader-operable (unlike a bare clickable div).
  import type { Snippet } from 'svelte';
  import Icon from '../Icon.svelte';
  import { mdiChevronDown, mdiChevronRight } from '../icons';

  interface Props {
    title: string;
    meta?: Snippet;
    open?: boolean;
    tone?: 'danger' | '';
    children: Snippet;
  }
  let { title, meta, open = $bindable(false), tone = '', children }: Props = $props();
  const id = `c${Math.random().toString(36).slice(2, 9)}`;
</script>

<div class="collapsible" class:tone-danger-edge={tone === 'danger'}>
  <button class="collapsible-head" aria-expanded={open} aria-controls={open ? id : undefined} onclick={() => (open = !open)}>
    <Icon path={open ? mdiChevronDown : mdiChevronRight} size={14} />
    <span class="collapsible-title">{title}</span>
    {#if meta}<span class="collapsible-meta">{@render meta()}</span>{/if}
  </button>
  {#if open}
    <div class="collapsible-body" {id}>{@render children()}</div>
  {/if}
</div>
