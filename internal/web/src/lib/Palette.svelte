<script lang="ts">
  // The Cmd/Ctrl+K command palette: jump to any page from anywhere. Starts as
  // a static registry of routes (global pages, plus the current tenant's
  // pages when the active route is inside one); later tasks can extend the
  // registry with real search results (repos, pulls, reviews).
  import type { Route } from './router.svelte';
  import { router, navigate } from './router.svelte';
  import { palette, togglePalette } from './keyboard.svelte';
  import Icon from './Icon.svelte';
  import type { Me, Page, Pull } from './types';
  import { getJSON } from './api.svelte';
  import { pullRoute } from './links';
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

  let { me }: { me: Me | undefined } = $props();

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

  // Gated the same way as the top-bar (App.svelte): operator console and
  // per-tenant admin are role-restricted, and "Sign in" only makes sense
  // when there's no session yet.
  function buildEntries(r: Route): Entry[] {
    const entries: Entry[] = [{ label: 'Overview', route: { name: 'overview' }, icon: mdiViewDashboardOutline }];
    if (me?.operator) {
      entries.push({ label: 'Operator console', route: { name: 'operator' }, icon: mdiConsoleLine });
    }
    if (!me) {
      entries.push({ label: 'Sign in', route: { name: 'signin' }, icon: mdiLogin });
    }
    // Every tenant the user can see gets its pages, current tenant first,
    // so any page of any tenant is a few keystrokes away.
    const current = currentSlug(r);
    const slugs = [...new Set([...(current ? [current] : []), ...(me?.tenants.map((t) => t.slug) ?? [])])];
    for (const slug of slugs) {
      entries.push(
        { label: 'Tenant overview', hint: slug, route: { name: 'tenant', slug }, icon: mdiViewDashboardOutline },
        { label: 'Repos', hint: slug, route: { name: 'repos', slug }, icon: mdiSourceRepository },
        { label: 'Pull requests', hint: slug, route: { name: 'pulls', slug }, icon: mdiSourcePull },
        { label: 'Queue', hint: slug, route: { name: 'queue', slug }, icon: mdiTrayFull },
        { label: 'Usage', hint: slug, route: { name: 'usage', slug }, icon: mdiCurrencyUsd },
        { label: 'Follow-ups', hint: slug, route: { name: 'followups', slug }, icon: mdiClipboardTextClockOutline },
      );
      if (me?.tenants.find((t) => t.slug === slug)?.role === 'admin') {
        entries.push({ label: 'Admin', hint: slug, route: { name: 'admin', slug }, icon: mdiCogOutline });
      }
    }
    for (const p of recent) {
      entries.push({ label: p.title, hint: `${p.repository}#${p.number}`, route: pullRoute(recentSlug, p), icon: mdiSourcePull });
    }
    return entries;
  }

  // The current tenant's recently updated pulls, fetched each time the
  // palette opens so they are jump targets too.
  let recent = $state<Pull[]>([]);
  let recentSlug = $state('');

  let recentSeq = 0;

  async function loadRecent(slug: string): Promise<void> {
    const seq = ++recentSeq;
    try {
      const p = await getJSON<Page<Pull>>(`/api/v1/tenants/${encodeURIComponent(slug)}/pulls?state=all&limit=20`);
      if (seq !== recentSeq) return;
      recent = p.items;
      recentSlug = slug;
    } catch (err) {
      console.error('palette recent pulls:', err);
    }
  }

  $effect(() => {
    const slug = currentSlug(router.route) ?? me?.tenants[0]?.slug;
    if (palette.open && slug) void loadRecent(slug);
  });

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
        {#each rows as row, i (i)}
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
