<script lang="ts">
  // A modal dialog on the native <dialog>: showModal() makes the rest of the
  // page inert, which is the focus trap, and Escape fires "cancel", which
  // closes it. Focus returns to whatever opened it. The body renders only
  // while open, so an input inside (a password, say) never outlives it.
  import type { Snippet } from 'svelte';

  interface Props {
    open: boolean;
    title: string;
    children: Snippet;
    footer?: Snippet;
    onclose?: () => void;
    // Where focus goes on close when the opener is gone from the page (a
    // save that remounted it, or a disabled button that dropped focus to
    // the body): a selector, by default the page title.
    fallback?: string;
  }
  let { open = $bindable(false), title, children, footer, onclose, fallback = 'main h1' }: Props = $props();
  const id = `d${Math.random().toString(36).slice(2, 9)}`;
  let el = $state<HTMLDialogElement | undefined>(undefined);
  let restore: HTMLElement | null = null;

  $effect(() => {
    if (!el) return;
    if (open && !el.open) {
      restore = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      el.showModal();
    } else if (!open && el.open) {
      el.close();
    }
  });

  function closed(): void {
    open = false;
    onclose?.();
    if (restore && restore !== document.body && restore.isConnected) {
      restore.focus();
    } else {
      const el = document.querySelector<HTMLElement>(fallback) ?? document.querySelector<HTMLElement>('main h1');
      if (el) {
        if (!el.hasAttribute('tabindex')) el.setAttribute('tabindex', '-1');
        el.focus();
      }
    }
    restore = null;
  }
</script>

<dialog bind:this={el} class="dialog" aria-modal="true" aria-labelledby={id} onclose={closed}>
  {#if open}
    <h2 class="dialog-title" {id}>{title}</h2>
    <div class="dialog-body">{@render children()}</div>
    {#if footer}<div class="dialog-actions">{@render footer()}</div>{/if}
  {/if}
</dialog>
