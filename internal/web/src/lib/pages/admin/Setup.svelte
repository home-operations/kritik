<script lang="ts">
  // The setup wizard's host: it opens the wizard for an admin while the
  // instance cannot review and they have not closed it, offers a banner to
  // resume once they have, and shows finished GitHub App registrations,
  // over the wizard, once.
  import { onMount, tick } from 'svelte';
  import { getJSON, sendJSON } from '../../api.svelte';
  import { notReviewing, setFlag, setupFlags } from '../../setup.svelte';
  import type { AppManifestResult, SetupStatus } from '../../types';
  import SetupWizard from './SetupWizard.svelte';
  import AppRegistrations from './AppRegistrations.svelte';

  let status = $state<SetupStatus | undefined>(undefined);
  let open = $state(false);
  let results = $state<AppManifestResult[]>([]);
  const reason = $derived(status ? notReviewing(status) : '');

  onMount(() => {
    void (async () => {
      try {
        status = await getJSON<SetupStatus>('/api/v1/operator/setup');
        if (reason && !setupFlags.dismissed) open = true;
      } catch {
        // No wizard without a status; the registrations still show.
      }
      // Opened after the wizard, so its dialog is on top.
      await tick();
      try {
        const r = await sendJSON<AppManifestResult[]>('POST', '/api/v1/app/manifests/collect');
        if (r?.length) results = r;
      } catch {
        // Nothing to show.
      }
    })();
  });

  async function refresh(done?: (s: SetupStatus) => boolean): Promise<void> {
    for (let i = 0; i < 20; i++) {
      status = await getJSON<SetupStatus>('/api/v1/operator/setup');
      if (!done || done(status)) return;
      await new Promise((r) => setTimeout(r, 300));
    }
  }

  function resume(): void {
    setFlag('dismissed', false);
    open = true;
  }
</script>

{#if status && reason && !open}
  <div class="setup-banner" role="note">
    <span>kritik cannot review yet: {reason}.</span>
    <button class="btn btn-small btn-primary" onclick={resume}>Resume setup</button>
  </div>
{/if}

{#if status}<SetupWizard bind:open {status} {refresh} />{/if}

<AppRegistrations bind:results />
