<script lang="ts">
  // The sub-tabs of a section's list pages, which stand in for a page
  // title: the h1 names the current one for assistive technology.
  import { href } from '../router.svelte';
  import { SECTIONS, SUB_TABS, type Section } from '../sections';
  import type { Route } from '../routes';

  let { section, slug, current }: { section: Section; slug: string; current: Route['name'] } = $props();

  const tabs = $derived(SUB_TABS[section] ?? []);
  const title = $derived(tabs.find((t) => t.name === current)?.label ?? SECTIONS[section].label);
</script>

<header class="page-head section-head">
  <h1 class="sr-only">{title}</h1>
  <nav class="tabs" aria-label={SECTIONS[section].label}>
    {#each tabs as t (t.name)}
      <a class="tab" class:active={t.name === current} aria-current={t.name === current ? 'page' : undefined} href={href(t.route(slug))}>
        {t.label}
      </a>
    {/each}
  </nav>
</header>
