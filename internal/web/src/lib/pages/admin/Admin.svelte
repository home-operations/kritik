<script lang="ts">
  import { href } from '../../router.svelte';
  import { canAdmin, session } from '../../session.svelte';
  import AuditTable from '../../components/AuditTable.svelte';
  import ConfigSection from './ConfigSection.svelte';
  import MembersSection from './MembersSection.svelte';

  let { slug, section }: { slug: string; section?: string } = $props();

  const SECTIONS = [
    ['config', 'Configuration'],
    ['members', 'Members'],
    ['audit', 'Audit log'],
  ] as const;
  const current = $derived(section ?? 'config');
  const known = $derived(SECTIONS.some(([s]) => s === current));
</script>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>Admin <span class="muted mono">{slug}</span></h1>
    </header>
    {#if !session.me}
      <p class="state-msg" aria-live="polite">Loading…</p>
    {:else if !canAdmin(slug)}
      <p class="state-msg" role="alert">Only a tenant admin or an instance operator can see this page.</p>
    {:else}
      <nav class="tabs" aria-label="Admin sections">
        {#each SECTIONS as [s, label] (s)}
          <a class="tab" class:active={s === current} aria-current={s === current ? 'page' : undefined} href={href({ name: 'admin', slug, section: s })}>
            {label}
          </a>
        {/each}
      </nav>
      <section class="tab-panel">
        {#if !known}
          <p class="state-msg">No such section.</p>
        {:else if current === 'config'}
          <ConfigSection {slug} />
        {:else if current === 'members'}
          <MembersSection {slug} />
        {:else}
          <section class="panel" aria-labelledby="admin-audit">
            <header class="panel-head"><h2 id="admin-audit">Audit log</h2></header>
            <AuditTable path={`/api/v1/tenants/${encodeURIComponent(slug)}/audit`} />
          </section>
        {/if}
      </section>
    {/if}
  </div>
</main>
