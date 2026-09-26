<script lang="ts">
  import { onMount } from 'svelte';
  import { basePath } from './lib/base';
  import { router, initRouter, href, navigate, parse, replace } from './lib/router.svelte';
  import { getJSON, sendJSON, ApiError, signinState } from './lib/api.svelte';
  import { initEvents, closeEvents } from './lib/events.svelte';
  import { theme, cycleTheme, initTheme } from './lib/theme.svelte';
  import { initClock } from './lib/time.svelte';
  import { initKeyboard, help, toggleHelp, togglePalette } from './lib/keyboard.svelte';
  import {
    mdiThemeLightDark,
    mdiWeatherNight,
    mdiWhiteBalanceSunny,
    mdiKeyboardOutline,
    mdiMagnify,
    mdiViewDashboardOutline,
    mdiViewGridOutline,
    mdiSourceRepository,
    mdiSourcePull,
    mdiTrayFull,
    mdiCurrencyUsd,
    mdiClipboardTextClockOutline,
    mdiCogOutline,
    mdiConsoleLine,
    mdiAccountOutline,
    mdiLogout,
    mdiChevronDown,
  } from './lib/icons';
  import Icon from './lib/Icon.svelte';
  import Palette from './lib/Palette.svelte';
  import SignIn from './lib/SignIn.svelte';
  import Page from './lib/pages/Page.svelte';
  import Toasts from './lib/components/Toasts.svelte';
  import { session, loadMeta } from './lib/session.svelte';
  import type { Me } from './lib/types';

  const me = $derived(session.me);

  onMount(() => {
    initTheme();
    initRouter();
    initKeyboard();
    initClock();
    void loadMeta();
    void loadMe();
  });

  async function loadMe(): Promise<void> {
    try {
      session.me = await getJSON<Me>('/api/v1/me');
      initEvents();
    } catch (err) {
      // A 401 already redirected to #/signin (see api.svelte.ts); anything
      // else leaves `me` unset and the shell renders signed-out.
      if (!(err instanceof ApiError && err.status === 401)) console.error('load me:', err);
    }
  }

  // A signed-in user never sits on the sign-in card, however they got there
  // (page load or in-app navigation): send them where a 401 bounced them
  // from, or the overview.
  $effect(() => {
    if (!me || router.route.name !== 'signin') return;
    const back = parse(signinState.returnTo || '#/');
    signinState.returnTo = '';
    replace(back.name === 'signin' ? { name: 'overview' } : back);
  });

  async function signOut(): Promise<void> {
    try {
      await sendJSON('POST', '/auth/logout');
    } catch (err) {
      console.error('sign out:', err);
    }
    closeEvents();
    session.me = undefined;
    navigate({ name: 'signin' });
  }

  // currentSlug reads the tenant slug off whatever route is active, falling
  // back to the first tenant so the nav has somewhere to point before the
  // user has ever picked one explicitly.
  const currentSlug = $derived('slug' in router.route ? router.route.slug : me?.tenants[0]?.slug);
  const currentTenant = $derived(me?.tenants.find((t) => t.slug === currentSlug));

  function switchTenant(slug: string): void {
    navigate({ name: 'tenant', slug });
  }

  const themeIconPath = $derived(
    theme.pref === 'auto' ? mdiThemeLightDark : theme.pref === 'dark' ? mdiWeatherNight : mdiWhiteBalanceSunny,
  );

  // Keep the help dialog's Tab from escaping to the page behind the backdrop;
  // Escape (global handler) and the backdrop close it.
  function trapTab(e: KeyboardEvent): void {
    if (e.key === 'Tab') e.preventDefault();
  }

  function focusOnMount(node: HTMLElement): void {
    node.focus();
  }

  // A native <details> has no built-in Escape handling and stays open on an
  // outside click, so both are wired up by hand here.
  let accountMenuEl = $state<HTMLDetailsElement | undefined>(undefined);

  function closeAccountMenu(): void {
    if (accountMenuEl) accountMenuEl.open = false;
  }

  function onAccountMenuKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.stopPropagation();
      closeAccountMenu();
    }
  }

  function onDocumentClick(e: MouseEvent): void {
    if (accountMenuEl?.open && !accountMenuEl.contains(e.target as Node)) closeAccountMenu();
  }
</script>

<svelte:window onclick={onDocumentClick} />

{#if router.route.name === 'signin'}
  <SignIn />
{:else}
  <div class="app">
    <header class="topbar">
      <a class="brand" href="#/">
        <img src="{basePath}/favicon.svg" width="22" height="22" alt="" />
        <span class="wordmark">kritik</span>
      </a>

      <div class="spacer"></div>

      <div class="actions">
        <button class="btn btn-icon" onclick={togglePalette} title="Go to (Ctrl/⌘ K)">
          <Icon path={mdiMagnify} label="Go to" />
        </button>
        <button class="btn btn-icon" onclick={toggleHelp} title="Keyboard shortcuts (?)">
          <Icon path={mdiKeyboardOutline} label="Keyboard shortcuts" />
        </button>
        <button class="btn btn-icon" onclick={cycleTheme} title={`Theme: ${theme.pref}`}>
          <Icon path={themeIconPath} label="Toggle theme" />
        </button>
        {#if me}
          <details class="account-menu" bind:this={accountMenuEl} onkeydown={onAccountMenuKeydown}>
            <summary class="btn btn-icon" title={me.account.displayName}>
              <Icon path={mdiAccountOutline} label="Account" />
              <Icon path={mdiChevronDown} size={12} />
            </summary>
            <div class="account-panel">
              <p class="account-name">{me.account.displayName}</p>
              <p class="account-email mono">{me.account.email}</p>
              <button class="btn" onclick={signOut}>
                <Icon path={mdiLogout} size={14} /> Sign out
              </button>
            </div>
          </details>
        {/if}
      </div>
    </header>

    <div class="shell">
      {#if me}
        <aside class="sidebar" aria-label="Navigation">
          <nav class="nav" aria-label="Home">
            <a
              class:active={router.route.name === 'overview'}
              aria-current={router.route.name === 'overview' ? 'page' : undefined}
              href={href({ name: 'overview' })}
            >
              <Icon path={mdiViewGridOutline} size={15} /> All tenants
            </a>
          </nav>
          {#if me.tenants.length > 0}
            <select
              class="tenant-switch"
              aria-label="Switch tenant"
              value={currentSlug}
              onchange={(e) => switchTenant(e.currentTarget.value)}
            >
              {#each me.tenants as t (t.slug)}
                <option value={t.slug}>{t.slug}</option>
              {/each}
            </select>
          {/if}

          {#if currentSlug}
            <nav class="nav" aria-label="Tenant">
              <a
                class:active={router.route.name === 'tenant'}
                aria-current={router.route.name === 'tenant' ? 'page' : undefined}
                href={href({ name: 'tenant', slug: currentSlug })}
              >
                <Icon path={mdiViewDashboardOutline} size={15} /> Overview
              </a>
              <a
                class:active={router.route.name === 'repos'}
                aria-current={router.route.name === 'repos' ? 'page' : undefined}
                href={href({ name: 'repos', slug: currentSlug })}
              >
                <Icon path={mdiSourceRepository} size={15} /> Repos
              </a>
              <a
                class:active={router.route.name === 'pulls'}
                aria-current={router.route.name === 'pulls' ? 'page' : undefined}
                href={href({ name: 'pulls', slug: currentSlug })}
              >
                <Icon path={mdiSourcePull} size={15} /> Pulls
              </a>
              <a
                class:active={router.route.name === 'queue'}
                aria-current={router.route.name === 'queue' ? 'page' : undefined}
                href={href({ name: 'queue', slug: currentSlug })}
              >
                <Icon path={mdiTrayFull} size={15} /> Queue
              </a>
              <a
                class:active={router.route.name === 'usage'}
                aria-current={router.route.name === 'usage' ? 'page' : undefined}
                href={href({ name: 'usage', slug: currentSlug })}
              >
                <Icon path={mdiCurrencyUsd} size={15} /> Usage
              </a>
              <a
                class:active={router.route.name === 'followups'}
                aria-current={router.route.name === 'followups' ? 'page' : undefined}
                href={href({ name: 'followups', slug: currentSlug })}
              >
                <Icon path={mdiClipboardTextClockOutline} size={15} /> Follow-ups
              </a>
              {#if currentTenant?.role === 'admin' || me?.operator}
                <a
                  class:active={router.route.name === 'admin'}
                  aria-current={router.route.name === 'admin' ? 'page' : undefined}
                  href={href({ name: 'admin', slug: currentSlug })}
                >
                  <Icon path={mdiCogOutline} size={15} /> Admin
                </a>
              {/if}
            </nav>
          {/if}
          {#if me.operator}
            <nav class="nav nav-instance" aria-label="Instance">
              <a
                class:active={router.route.name === 'operator'}
                aria-current={router.route.name === 'operator' ? 'page' : undefined}
                href={href({ name: 'operator' })}
              >
                <Icon path={mdiConsoleLine} size={15} /> Operator console
              </a>
            </nav>
          {/if}
        </aside>
      {/if}

      <Page route={router.route} />
    </div>

    <Palette {me} />

    <Toasts />

    {#if help.open}
      <div class="help-overlay">
        <button class="help-backdrop" aria-label="Close keyboard shortcuts" onclick={toggleHelp}></button>
        <div
          class="help-card"
          role="dialog"
          aria-modal="true"
          aria-label="Keyboard shortcuts"
          tabindex="-1"
          use:focusOnMount
          onkeydown={trapTab}
        >
          <h2>Keyboard shortcuts</h2>
          <dl class="help-keys">
            <dt><kbd>Ctrl</kbd>/<kbd>⌘</kbd> <kbd>k</kbd></dt>
            <dd>go to a page</dd>
            <dt><kbd>?</kbd></dt>
            <dd>toggle this help</dd>
          </dl>
        </div>
      </div>
    {/if}
  </div>
{/if}
