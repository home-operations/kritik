<script lang="ts">
  // The Cmd/Ctrl+K command palette: jump to any page from anywhere. Starts as
  // a static registry of routes (global pages, plus the current tenant's
  // pages when the active route is inside one); later tasks can extend the
  // registry with real search results (repos, pulls, reviews).
  import type { Route } from './router.svelte';
  import { router, navigate } from './router.svelte';
  import { palette, togglePalette } from './keyboard.svelte';
  import Icon from './Icon.svelte';
  import {
    mdiMagnify,
    mdiViewDashboardOutline,
    mdiLogin,
    mdiConsoleLine,
    mdiSourceRepository,
    mdiSourcePull,
    mdiTrayFull,
    mdiCurrencyUsd,
    mdiClipboardTextClockOutline,
    mdiCogOutline,
  } from './icons';

  interface Entry {
    label: string;
    hint?: string;
    route: Route;
    icon: string;
  }

  // currentSlug reads the tenant slug off whatever route is active, when the
  // route carries one — every tenant-scoped Route variant does.
  function currentSlug(r: Route): string | undefined {
    return 'slug' in r ? r.slug : undefined;
  }

  function buildEntries(r: Route): Entry[] {
    const entries: Entry[] = [
      { label: 'Overview', route: { name: 'overview' }, icon: mdiViewDashboardOutline },
      { label: 'Operator console', route: { name: 'operator' }, icon: mdiConsoleLine },
      { label: 'Sign in', route: { name: 'signin' }, icon: mdiLogin },
    ];
    const slug = currentSlug(r);
    if (slug) {
      entries.push(
        { label: 'Tenant overview', hint: slug, route: { name: 'tenant', slug }, icon: mdiViewDashboardOutline },
        { label: 'Repos', hint: slug, route: { name: 'repos', slug }, icon: mdiSourceRepository },
        { label: 'Pull requests', hint: slug, route: { name: 'pulls', slug }, icon: mdiSourcePull },
        { label: 'Queue', hint: slug, route: { name: 'queue', slug }, icon: mdiTrayFull },
        { label: 'Usage', hint: slug, route: { name: 'usage', slug }, icon: mdiCurrencyUsd },
        { label: 'Follow-ups', hint: slug, route: { name: 'followups', slug }, icon: mdiClipboardTextClockOutline },
        { label: 'Admin', hint: slug, route: { name: 'admin', slug }, icon: mdiCogOutline },
      );
    }
    return entries;
  }

  let q = $state('');
  let idx = $state(0);

  // Fresh state on every open: the component stays mounted between opens, so
  // drop the previous query.
  $effect(() => {
    if (palette.open) q = '';
  });

  const rows = $derived.by(() => {
    const needle = q.trim().toLowerCase();
    const entries = buildEntries(router.route);
    if (!needle) return entries;
    return entries.filter((e) => e.label.toLowerCase().includes(needle) || e.hint?.toLowerCase().includes(needle));
  });

  // Clamp the cursor when the rows change under it (typing narrows the list).
  $effect(() => {
    if (idx >= rows.length) idx = Math.max(0, rows.length - 1);
  });

  function commit(row: Entry | undefined): void {
    if (!row) return;
    togglePalette();
    navigate(row.route);
  }

  function onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      togglePalette();
    } else if (e.key === 'ArrowDown' || (e.key === 'Tab' && !e.shiftKey)) {
      idx = rows.length ? (idx + 1) % rows.length : 0;
    } else if (e.key === 'ArrowUp' || (e.key === 'Tab' && e.shiftKey)) {
      idx = rows.length ? (idx - 1 + rows.length) % rows.length : 0;
    } else if (e.key === 'Enter') {
      commit(rows[idx]);
    } else {
      return;
    }
    e.preventDefault();
  }

  function focusOnMount(node: HTMLElement): void {
    node.focus();
  }
</script>

{#if palette.open}
  <div class="palette-overlay">
    <button class="help-backdrop" aria-label="Close palette" onclick={togglePalette}></button>
    <!-- The keydown handler lives on the dialog (not the input) so Tab cycles
         the rows — and never escapes to the page behind — wherever focus sits
         inside the palette. aria-modal marks the background inert for AT. -->
    <div class="palette" role="dialog" aria-modal="true" aria-label="Go to" tabindex="-1" onkeydown={onKeydown}>
      <div class="palette-input">
        <Icon path={mdiMagnify} size={16} />
        <!-- svelte-ignore a11y_autofocus -->
        <input
          bind:value={q}
          use:focusOnMount
          oninput={() => (idx = 0)}
          placeholder="Go to…"
          aria-label="Go to"
        />
        <span class="palette-esc"><kbd>Esc</kbd></span>
      </div>

      <div class="palette-body">
        {#each rows as row, i (row.label + (row.hint ?? ''))}
          <button
            class="palette-row"
            class:active={i === idx}
            onclick={() => commit(row)}
            onmouseenter={() => (idx = i)}
          >
            <Icon path={row.icon} size={14} />
            <span class="row-main">
              <span class="row-title">{row.label}</span>
              {#if row.hint}<span class="row-sub mono">{row.hint}</span>{/if}
            </span>
          </button>
        {/each}
        {#if q.trim() && !rows.length}
          <p class="palette-empty">Nothing matches “{q}”.</p>
        {/if}
      </div>

      <div class="palette-footer">
        <span><kbd>↑</kbd><kbd>↓</kbd> navigate</span>
        <span><kbd>⏎</kbd> open</span>
      </div>
    </div>
  </div>
{/if}
