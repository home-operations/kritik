<script lang="ts">
  // The Settings section's own navigation: the account's pages, then, for an
  // admin, the instance's.
  import { href } from '../router.svelte';
  import type { Route } from '../routes';
  import { session } from '../session.svelte';
  import Icon from '../Icon.svelte';
  import { mdiSourceRepository, mdiTuneVariant, mdiClipboardTextClockOutline, mdiConsoleLine } from '../icons';

  let { route }: { route: Route } = $props();

  const slug = $derived('slug' in route ? route.slug : session.me?.accounts[0]);
  const admin = $derived(session.me?.admin === true);
  const adminSection = $derived(route.name === 'admin' ? (route.section ?? 'config') : undefined);

  interface Item {
    label: string;
    icon: string;
    to: Route;
    active: boolean;
  }

  const accountItems = $derived.by((): Item[] => {
    if (!slug) return [];
    const items: Item[] = [
      { label: 'Repositories', icon: mdiSourceRepository, to: { name: 'repos', slug }, active: route.name === 'repos' || route.name === 'repo' },
    ];
    if (admin) {
      items.push(
        { label: 'Configuration', icon: mdiTuneVariant, to: { name: 'admin', slug, section: 'config' }, active: adminSection === 'config' },
        { label: 'Audit log', icon: mdiClipboardTextClockOutline, to: { name: 'admin', slug, section: 'audit' }, active: adminSection === 'audit' },
      );
    }
    return items;
  });
</script>

<nav class="subnav" aria-label="Settings">
  {#if slug}
    <p class="subnav-group mono">{slug}</p>
    {#each accountItems as it (it.label)}
      <a class="subnav-item" class:active={it.active} aria-current={it.active ? 'page' : undefined} href={href(it.to)}>
        <Icon path={it.icon} size={15} />
        {it.label}
      </a>
    {/each}
  {/if}
  {#if admin}
    <p class="subnav-group">Instance</p>
    <a class="subnav-item" class:active={route.name === 'console'} aria-current={route.name === 'console' ? 'page' : undefined} href={href({ name: 'console' })}>
      <Icon path={mdiConsoleLine} size={15} />
      Admin console
    </a>
  {/if}
</nav>
