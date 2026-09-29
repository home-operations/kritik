<script lang="ts">
  // The Settings section's own navigation: the account's pages, then, for an
  // admin, the instance's. The page shown lists its parts under it, and a
  // search finds a setting by name and lands on its field.
  import { href } from '../router.svelte';
  import type { Route } from '../routes';
  import { session } from '../session.svelte';
  import { focusWhenShown } from '../focus';
  import { ACCOUNT_FIELDS, ACCOUNT_SECTIONS, CONSOLE_SECTIONS, matches, type SettingEntry } from '../settingsindex';
  import Icon from '../Icon.svelte';
  import { mdiSourceRepository, mdiTuneVariant, mdiClipboardTextClockOutline, mdiConsoleLine, mdiMagnify } from '../icons';

  let { route }: { route: Route } = $props();

  const slug = $derived('slug' in route ? route.slug : session.me?.accounts[0]);
  const admin = $derived(session.me?.admin === true);
  const adminSection = $derived(route.name === 'admin' ? (route.section ?? 'config') : undefined);

  interface Item {
    label: string;
    icon: string;
    to: Route;
    active: boolean;
    // parts are the page's sections, listed while it is shown.
    parts?: readonly SettingEntry[];
  }

  const items = $derived.by((): { group: string; mono: boolean; items: Item[] }[] => {
    const groups: { group: string; mono: boolean; items: Item[] }[] = [];
    if (slug) {
      const account: Item[] = [
        { label: 'Repositories', icon: mdiSourceRepository, to: { name: 'repos', slug }, active: route.name === 'repos' || route.name === 'repo' },
      ];
      if (admin) {
        account.push(
          {
            label: 'Configuration',
            icon: mdiTuneVariant,
            to: { name: 'admin', slug, section: 'config' },
            active: adminSection === 'config',
            parts: ACCOUNT_SECTIONS,
          },
          { label: 'Audit log', icon: mdiClipboardTextClockOutline, to: { name: 'admin', slug, section: 'audit' }, active: adminSection === 'audit' },
        );
      }
      groups.push({ group: slug, mono: true, items: account });
    }
    if (admin) {
      groups.push({
        group: 'Instance',
        mono: false,
        items: [{ label: 'Admin console', icon: mdiConsoleLine, to: { name: 'console' }, active: route.name === 'console', parts: CONSOLE_SECTIONS }],
      });
    }
    return groups;
  });

  // The search's hits: each setting with the page it is on.
  let query = $state('');
  const hits = $derived.by(() => {
    if (!query.trim()) return [];
    const out: { entry: SettingEntry; to: Route; where: string }[] = [];
    if (slug) {
      for (const e of ACCOUNT_FIELDS) {
        if (matches(e, query)) out.push({ entry: e, to: { name: 'admin', slug, section: 'config' }, where: 'Configuration' });
      }
    }
    for (const e of CONSOLE_SECTIONS) if (matches(e, query)) out.push({ entry: e, to: { name: 'console' }, where: 'Admin console' });
    return out;
  });
</script>

<nav class="subnav" aria-label="Settings">
  {#if admin}
    <label class="subnav-search">
      <Icon path={mdiMagnify} size={14} />
      <span class="sr-only">Search settings</span>
      <input type="search" placeholder="Search settings" bind:value={query} />
    </label>
  {/if}
  {#if query.trim()}
    <p class="subnav-group">{hits.length ? 'Settings' : 'No setting by that name'}</p>
    {#each hits as h (h.where + h.entry.target)}
      <a class="subnav-item subnav-hit" href={href(h.to)} onclick={() => focusWhenShown(h.entry.target)}>
        <span>{h.entry.label}</span>
        <span class="small muted">{h.where}</span>
      </a>
    {/each}
  {:else}
    {#each items as g (g.group)}
      <p class="subnav-group" class:mono={g.mono}>{g.group}</p>
      {#each g.items as it (it.label)}
        <a class="subnav-item" class:active={it.active} aria-current={it.active ? 'page' : undefined} href={href(it.to)}>
          <Icon path={it.icon} size={15} />
          {it.label}
        </a>
        {#if it.active && it.parts}
          <ul class="subnav-parts" aria-label="{it.label} sections">
            {#each it.parts as p (p.target)}
              <li><a class="subnav-part" href={href(it.to)} onclick={() => focusWhenShown(p.target, true)}>{p.label}</a></li>
            {/each}
          </ul>
        {/if}
      {/each}
    {/each}
  {/if}
</nav>
