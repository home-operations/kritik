<script lang="ts">
  // Registers a GitHub App from a manifest (ADR-0014 §2.3): the form asks
  // the API for the manifest and posts it to GitHub, which sends the admin
  // back through kritik's callback to this page. Back here, the finished
  // registration is read once, with the App's client secret, which is
  // never shown again.
  import { onMount } from 'svelte';
  import { sendJSON } from '../../api.svelte';
  import { describe, errorPath } from '../../manage';
  import type { AppManifestForm, AppManifestRequest, AppManifestResult } from '../../types';
  import Dialog from '../../components/Dialog.svelte';
  import Copy from '../../components/Copy.svelte';

  let connection = $state('');
  let owner = $state<'user' | 'organization'>('user');
  let organization = $state('');
  let visibility = $state<'private' | 'public'>('private');
  let name = $state('');
  let busy = $state(false);
  let errMessage = $state('');
  let errPath = $state('');

  let results = $state<AppManifestResult[]>([]);
  let resultsOpen = $state(false);

  onMount(() => {
    void sendJSON<AppManifestResult[]>('POST', '/api/v1/app/manifests/collect')
      .then((r) => {
        if (!r?.length) return;
        results = r;
        resultsOpen = true;
      })
      .catch(() => undefined);
  });

  // post sends the browser to GitHub with the manifest, as the form GitHub
  // expects: a top-level POST whose manifest field is the JSON.
  function post(form: AppManifestForm): void {
    const el = document.createElement('form');
    el.method = 'POST';
    el.action = form.url;
    const field = document.createElement('input');
    field.type = 'hidden';
    field.name = 'manifest';
    field.value = JSON.stringify(form.manifest);
    el.append(field);
    document.body.append(el);
    el.submit();
  }

  async function submit(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    busy = true;
    errMessage = '';
    errPath = '';
    const req: AppManifestRequest = { connection: connection.trim(), public: visibility === 'public' };
    if (owner === 'organization') req.organization = organization.trim();
    if (name.trim()) req.name = name.trim();
    try {
      post(await sendJSON<AppManifestForm>('POST', '/api/v1/app/manifests', req));
    } catch (err) {
      errMessage = describe(err);
      errPath = errorPath(err);
      busy = false;
    }
  }
</script>

<section class="panel" aria-labelledby="op-app">
  <header class="panel-head"><h2 id="op-app">Create a GitHub App</h2></header>
  <div class="panel-body">
    <p class="field-hint">
      GitHub registers the App with the webhook, permissions and events kritik needs, and kritik adds it as a connection
      serving the account it belongs to. To serve more accounts, add them to the connection afterwards.
    </p>
    <form class="form" onsubmit={submit} novalidate>
      <div class="fields">
        <label class="field">
          <span>Connection name</span>
          <input class="mono" data-path="connection" aria-invalid={errPath === 'connection' || undefined} bind:value={connection} placeholder="github" required />
          <span class="field-hint">Its webhook is <span class="mono">/hooks/{connection.trim() || '<name>'}</span>.</span>
        </label>
        <label class="field">
          <span>App name on GitHub</span>
          <input bind:value={name} placeholder={`kritik-${connection.trim() || '<connection>'}`} />
          <span class="field-hint">Unique across GitHub; it can still be changed there.</span>
        </label>
      </div>
      <fieldset class="field">
        <legend>Register it under</legend>
        <label><input type="radio" name="app-owner" value="user" bind:group={owner} /> My GitHub account</label>
        <label><input type="radio" name="app-owner" value="organization" bind:group={owner} /> An organization</label>
        {#if owner === 'organization'}
          <input class="mono" aria-label="Organization" data-path="organization" aria-invalid={errPath === 'organization' || undefined} bind:value={organization} placeholder="org-1" />
        {/if}
      </fieldset>
      <fieldset class="field">
        <legend>Who may install it</legend>
        <label><input type="radio" name="app-visibility" value="private" bind:group={visibility} /> Private: only the account it belongs to</label>
        <label><input type="radio" name="app-visibility" value="public" bind:group={visibility} /> Public: any account</label>
        <span class="field-hint">
          Either way kritik reviews only the accounts its connection lists; a public App installed elsewhere is shown for removal.
        </span>
      </fieldset>
      <div aria-live="assertive">
        {#if errMessage}<div class="form-alert" role="alert"><span>{errMessage}</span></div>{/if}
      </div>
      <div class="form-actions">
        <button type="submit" class="btn btn-primary" disabled={busy}>{busy ? 'Going to GitHub…' : 'Create on GitHub'}</button>
      </div>
    </form>
  </div>
</section>

<Dialog bind:open={resultsOpen} title="GitHub App registration" onclose={() => (results = [])} fallback="#op-app">
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
