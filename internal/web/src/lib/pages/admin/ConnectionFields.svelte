<script lang="ts">
  import { untrack } from 'svelte';
  import { accountKey, canKeep, type ConnectionDraft, type SecretDraft } from '../../spec';
  import SecretField from './SecretField.svelte';
  import { hookURL } from '../../session.svelte';

  interface Props {
    inst: ConnectionDraft;
    index: number;
    inv: (path: string) => boolean;
    onremove: () => void;
  }
  let { inst = $bindable(), index, inv, onremove }: Props = $props();
  const p = $derived(`connections[${index}]`);

  // A kept key only stays valid for the forge and accounts it was issued
  // for; the server refuses the save otherwise (reenter_secret).
  const orig = untrack(() => ({ forge: inst.forge, accounts: accountKey(inst.accounts) }));
  const moved = $derived(inst.forge !== orig.forge || accountKey(inst.accounts) !== orig.accounts);
  const keepable = $derived(canKeep(inst));
  const re = (s: SecretDraft) =>
    moved && s.wasSet && keepable ? 'The forge or accounts changed: enter this secret again rather than keeping it.' : undefined;
</script>

<div class="item-card">
  <div class="item-head">
    <span class="mono">{inst.name || 'New connection'}</span>
    <button type="button" class="btn btn-small btn-danger" onclick={onremove}>Remove connection</button>
  </div>
  {#if inst.origName && !keepable}
    <p class="field-hint" role="note">Renamed from <span class="mono">{inst.origName}</span>: its stored secrets cannot be kept, so enter or generate each again.</p>
  {/if}
  <div class="fields">
    <label class="field">
      <span>Name</span>
      <input data-path="{p}.name" aria-invalid={inv(`${p}.name`) || undefined} bind:value={inst.name} required />
    </label>
    <label class="field">
      <span>Forge</span>
      <select data-path="{p}.forge" aria-invalid={inv(`${p}.forge`) || undefined} bind:value={inst.forge}>
        <option value="github">GitHub</option>
        <option value="github-enterprise" disabled>GitHub Enterprise Server (not yet supported)</option>
        <option value="gitlab" disabled>GitLab (not yet supported)</option>
        <option value="forgejo" disabled>Forgejo (not yet supported)</option>
        <option value="gitea" disabled>Gitea (not yet supported)</option>
      </select>
    </label>
    <label class="field">
      <span>Accounts (one per line)</span>
      <textarea rows="2" data-path="{p}.accounts" aria-invalid={inv(`${p}.accounts`) || undefined} bind:value={inst.accounts} required></textarea>
      <span class="field-hint">The users and organizations it serves; a webhook from any other is ignored.</span>
    </label>
  </div>
  <div class="fields">
    <label class="field">
      <span>App client ID</span>
      <input data-path="{p}.app.clientId" aria-invalid={inv(`${p}.app.clientId`) || undefined} bind:value={inst.clientId} />
    </label>
    {#if inst.clientIdFrom.wasSet || inst.clientIdFrom.mode !== 'none'}
      <SecretField label="App client ID (secret)" path="{p}.app.clientIdFrom" bind:secret={inst.clientIdFrom} optional {keepable} invalid={inv(`${p}.app.clientIdFrom`)} hint={re(inst.clientIdFrom)} />
    {/if}
    <SecretField label="App private key" path="{p}.app.privateKey" bind:secret={inst.privateKey} {keepable} invalid={inv(`${p}.app.privateKey`)} hint={re(inst.privateKey)} />
    <SecretField label="Webhook secret" path="{p}.app.webhookSecret" bind:secret={inst.appWebhookSecret} generatable {keepable} invalid={inv(`${p}.app.webhookSecret`)} />
  </div>
  <p class="field-hint">
    Webhook: set the GitHub App's webhook URL to <span class="mono">{hookURL(`/hooks/${inst.name || '<name>'}`)}</span>, with
    this webhook secret. It covers every repository the App is installed on.
  </p>
</div>
