<script lang="ts">
  import { canKeepKey, providerPath, type ProviderDraft } from '../../spec';
  import SecretField from './SecretField.svelte';
  import KeyTest from './KeyTest.svelte';

  interface Props {
    prov: ProviderDraft;
    inv: (path: string) => boolean;
    onremove: () => void;
    // account is the account, "<forge>/<name>", whose own key this is;
    // absent for the instance's.
    account?: string;
    onmodels?: (models: string[]) => void;
  }
  let { prov = $bindable(), inv, onremove, account, onmodels }: Props = $props();
  const p = $derived(providerPath(prov));
  // The server keeps a key only under its name and endpoint; the save is
  // refused otherwise (reenter_secret).
  const keepable = $derived(canKeepKey(prov));
</script>

<div class="item-card">
  <div class="item-head">
    <span class="mono">{prov.name || 'New provider key'}</span>
    <button type="button" class="btn btn-small btn-danger" onclick={onremove}>Remove provider key</button>
  </div>
  {#if prov.apiKey.wasSet && !keepable}
    <p class="field-hint" role="note">The name, type or endpoint changed, so the stored key cannot be kept: enter it again.</p>
  {/if}
  <div class="fields">
    <label class="field">
      <span>Name</span>
      <input class="mono" data-path="{p}.name" aria-invalid={inv(`${p}.name`) || undefined} bind:value={prov.name} required />
      <span class="field-hint">Models on it are <span class="mono">{prov.name || '<name>'}/&lt;model&gt;</span>.</span>
    </label>
    <label class="field">
      <span>Provider</span>
      <select data-path="{p}.type" aria-invalid={inv(`${p}.type`) || undefined} bind:value={prov.type}>
        <option value="openrouter">OpenRouter</option>
        <option value="openai">OpenAI</option>
        <option value="anthropic">Anthropic</option>
      </select>
    </label>
    <label class="field">
      <span>Base URL</span>
      <input class="mono" data-path="{p}.baseUrl" aria-invalid={inv(`${p}.baseUrl`) || undefined} bind:value={prov.baseUrl} placeholder="the provider's own endpoint" />
      <span class="field-hint">Blank uses the provider's own endpoint; another must be https on a host an admin allows.</span>
    </label>
    <SecretField label="API key" path="{p}.apiKey" bind:secret={prov.apiKey} {keepable} invalid={inv(`${p}.apiKey`)} />
  </div>
  <KeyTest
    path="/api/v1/admin/providers/test"
    secret={prov.apiKey}
    {keepable}
    {onmodels}
    request={(apiKey) => ({ type: prov.type, baseUrl: prov.baseUrl.trim() || undefined, apiKey, name: prov.origName || undefined, account })}
  />
</div>
