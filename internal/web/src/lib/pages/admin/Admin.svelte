<script lang="ts">
  import { accountApi } from '../../links';
  import { isAdmin, session } from '../../session.svelte';
  import AuditTable from '../../components/AuditTable.svelte';

  let { slug, section }: { slug: string; section?: string } = $props();

  const SECTIONS: Record<string, string> = { audit: 'Audit log' };
  const current = $derived(section ?? 'audit');
  const title = $derived(Object.hasOwn(SECTIONS, current) ? SECTIONS[current] : 'Settings');
</script>

<svelte:head><title>{title} · {slug} · kritik</title></svelte:head>

<main class="page">
  <div class="page-inner">
    <header class="page-head">
      <h1>{title}</h1>
    </header>
    {#if !session.me}
      <p class="state-msg" aria-live="polite">Loading…</p>
    {:else if !isAdmin()}
      <p class="state-msg" role="alert">Only an admin can see this page.</p>
    {:else if !Object.hasOwn(SECTIONS, current)}
      <p class="state-msg">No such section.</p>
    {:else}
      <section class="panel">
        <AuditTable path={`${accountApi(slug)}/audit`} />
      </section>
    {/if}
  </div>
</main>
