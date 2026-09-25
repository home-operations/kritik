<script lang="ts">
  import { untrack } from 'svelte';
  import { canKeep, type InstallationDraft, type SecretDraft } from '../../spec';
  import SecretField from './SecretField.svelte';

  interface Props {
    inst: InstallationDraft;
    index: number;
    inv: (path: string) => boolean;
    onremove: () => void;
  }
  let { inst = $bindable(), index, inv, onremove }: Props = $props();
  const p = $derived(`installations[${index}]`);

  // A kept token only stays valid for the forge, host and account it was
  // issued for; the server refuses the save otherwise (reenter_secret).
  const orig = untrack(() => ({ forge: inst.forge, host: inst.host, account: inst.account }));
  const moved = $derived(inst.forge !== orig.forge || inst.host !== orig.host || inst.account !== orig.account);
  const keepable = $derived(canKeep(inst));
  const re = (s: SecretDraft) =>
    moved && s.wasSet && keepable ? 'The forge, host or account changed: enter this secret again rather than keeping it.' : undefined;
</script>

<div class="item-card">
  <div class="item-head">
    <span class="mono">{inst.name || 'New installation'}</span>
    <button type="button" class="btn btn-small btn-danger" onclick={onremove}>Remove installation</button>
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
        <option value="gitlab">GitLab</option>
        <option value="forgejo">Forgejo</option>
      </select>
    </label>
    <label class="field">
      <span>Host</span>
      <input data-path="{p}.host" aria-invalid={inv(`${p}.host`) || undefined} bind:value={inst.host} placeholder={inst.forge === 'github' ? 'github.com' : 'https://forge.example'} />
      <span class="field-hint">https only; blank means github.com for GitHub.</span>
    </label>
    <label class="field">
      <span>Account</span>
      <input data-path="{p}.account" aria-invalid={inv(`${p}.account`) || undefined} bind:value={inst.account} required />
    </label>
  </div>
  {#if inst.forge === 'github'}
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
  {:else}
    <div class="fields">
      <SecretField label="Token" path="{p}.token" bind:secret={inst.token} {keepable} invalid={inv(`${p}.token`)} hint={re(inst.token)} />
      <SecretField label="Webhook secret" path="{p}.webhookSecret" bind:secret={inst.webhookSecret} generatable {keepable} invalid={inv(`${p}.webhookSecret`)} />
      <SecretField label="Git token" path="{p}.gitToken" bind:secret={inst.gitToken} optional {keepable} invalid={inv(`${p}.gitToken`)} hint={re(inst.gitToken) ?? 'Optional: a separate token for git clones.'} />
    </div>
  {/if}
  <p class="field-hint">Webhook path: <span class="mono">/hooks/{inst.name || '<name>'}</span></p>
</div>
