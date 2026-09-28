<script lang="ts">
  // The form that registers a GitHub App from a manifest (ADR-0014 §2.3):
  // it asks the API for the manifest and posts it to GitHub, which sends
  // the admin back through kritik's callback. AppRegistrations shows what
  // came of it.
  import { sendJSON } from '../../api.svelte';
  import { describe, errorPath } from '../../manage';
  import type { AppManifestForm, AppManifestRequest } from '../../types';

  let connection = $state('');
  let owner = $state<'user' | 'organization'>('user');
  let organization = $state('');
  let visibility = $state<'private' | 'public'>('private');
  let name = $state('');
  let busy = $state(false);
  let errMessage = $state('');
  let errPath = $state('');

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
