<script lang="ts">
  // The audit log, newest first, one keyset page at a time.
  import { Paged } from '../resource.svelte';
  import type { AuditEvent } from '../types';
  import StateView from './StateView.svelte';
  import LoadMore from './LoadMore.svelte';
  import Time from './Time.svelte';
  import Collapsible from './Collapsible.svelte';

  let { path, showTenant = false }: { path: string; showTenant?: boolean } = $props();

  const paged = new Paged<AuditEvent>(
    (cursor) => (cursor ? `${path}?cursor=${encodeURIComponent(cursor)}` : path),
    (e) => e.id,
  );
  $effect(() => {
    void paged.load();
  });

  const hasDetail = (e: AuditEvent) => Object.keys(e.detail ?? {}).length > 0;
</script>

<StateView res={paged.first} retry={() => paged.load()} isEmpty={(p) => p.items.length === 0} empty="No audit events yet.">
  {#snippet children()}
    <div class="table-wrap">
      <table class="data audit">
        <thead>
          <tr>
            <th scope="col">Time</th>
            {#if showTenant}<th scope="col">Tenant</th>{/if}
            <th scope="col">Actor</th>
            <th scope="col">Action</th>
            <th scope="col">Target</th>
            <th scope="col">Detail</th>
          </tr>
        </thead>
        <tbody>
          {#each paged.items as e (e.id)}
            <tr>
              <td><Time iso={e.at} /></td>
              {#if showTenant}<td class="mono">{e.tenant || '—'}</td>{/if}
              <td>{e.actor?.displayName ?? 'system'}</td>
              <td class="mono">{e.action}</td>
              <td class="mono">{e.target}</td>
              <td>
                {#if hasDetail(e)}
                  <Collapsible title="detail">
                    <pre class="detail-json">{JSON.stringify(e.detail, null, 2)}</pre>
                  </Collapsible>
                {:else}<span class="muted">—</span>{/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <LoadMore {paged} />
  {/snippet}
</StateView>
