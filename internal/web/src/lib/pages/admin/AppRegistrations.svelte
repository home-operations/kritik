<script lang="ts">
  // The finished GitHub App registrations, read once: each App with its
  // install link and its client secret, which is never shown again, or why
  // it failed.
  import type { AppManifestResult } from '../../types';
  import Dialog from '../../components/Dialog.svelte';
  import Copy from '../../components/Copy.svelte';

  let { results = $bindable() }: { results: AppManifestResult[] } = $props();
  let resultsOpen = $state(false);
  $effect(() => {
    resultsOpen = results.length > 0;
  });
</script>

<Dialog bind:open={resultsOpen} title="GitHub App registration" onclose={() => (results = [])}>
  {#each results as r (r.connection)}
    <div class="item-card">
      <div class="item-head"><span class="mono">{r.connection}</span></div>
      {#if r.error}
        <p class="form-alert" role="alert">{r.error}</p>
      {/if}
      {#if r.slug}
        <p>
          GitHub registered <span class="mono">{r.slug}</span>, and connection <span class="mono">{r.connection}</span> now
          holds its credentials. <a href={r.installUrl} target="_blank" rel="noopener noreferrer">Install it on GitHub</a>
          to start reviewing.
        </p>
      {/if}
      {#if r.clientSecret}
        <p class="form-alert" role="note"><strong>This is the only time the client secret is shown.</strong> kritik does not keep it.</p>
        <p class="small">
          To sign in with GitHub through this App, set <span class="mono">auth.github.clientId</span> and
          <span class="mono">auth.github.clientSecret</span>, or <span class="mono">KRITIK_AUTH_GITHUB_CLIENT_ID</span> and
          <span class="mono">KRITIK_AUTH_GITHUB_CLIENT_SECRET</span>, to these.
        </p>
        <p class="small">Client ID: <span class="mono" data-testid="app-client-id">{r.clientId}</span></p>
        <div class="secret-once">
          <span class="mono" data-testid="app-client-secret">{r.clientSecret}</span>
          <Copy text={r.clientSecret} label="Copy client secret" />
        </div>
      {/if}
    </div>
  {/each}
  {#snippet footer()}
    <button class="btn btn-primary" onclick={() => (resultsOpen = false)}>Done</button>
  {/snippet}
</Dialog>
