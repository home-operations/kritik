<script lang="ts">
  import { canKeepEmbeddingKey, type EmbeddingDraft } from '../../spec';
  import SecretField from './SecretField.svelte';
  import KeyTest from './KeyTest.svelte';

  interface Props {
    emb: EmbeddingDraft;
    inv: (path: string) => boolean;
    onremove: () => void;
  }
  let { emb = $bindable(), inv, onremove }: Props = $props();
  // The server keeps the key only for the endpoint it was entered for; the
  // save is refused otherwise (reenter_secret).
  const keepable = $derived(canKeepEmbeddingKey(emb));
</script>

<div class="item-card">
  <div class="item-head">
    <span class="mono">{emb.model || 'New embedder'}</span>
    <button type="button" class="btn btn-small btn-danger" onclick={onremove}>Remove embedder</button>
  </div>
  {#if emb.apiKey.wasSet && !keepable}
    <p class="field-hint" role="note">The endpoint changed, so the stored key cannot be kept: enter it again.</p>
  {/if}
  <div class="fields">
    <label class="field">
      <span>Endpoint</span>
      <input class="mono" data-path="embedding.baseUrl" aria-invalid={inv('embedding.baseUrl') || undefined} bind:value={emb.baseUrl} placeholder="https://openrouter.ai/api/v1" required />
      <span class="field-hint">Any OpenAI-compatible embeddings endpoint.</span>
    </label>
    <label class="field">
      <span>Model</span>
      <input class="mono" data-path="embedding.model" aria-invalid={inv('embedding.model') || undefined} bind:value={emb.model} required />
    </label>
    <label class="field">
      <span>Dimension</span>
      <input inputmode="numeric" data-path="embedding.dims" aria-invalid={inv('embedding.dims') || undefined} bind:value={emb.dims} required />
      <span class="field-hint">At most 4000. A new model or dimension rebuilds every repository's index.</span>
    </label>
    <SecretField label="Embedding API key" path="embedding.apiKey" bind:secret={emb.apiKey} {keepable} invalid={inv('embedding.apiKey')} />
  </div>
  <KeyTest
    path="/api/v1/admin/embedding/test"
    secret={emb.apiKey}
    {keepable}
    request={(apiKey) => ({ baseUrl: emb.baseUrl.trim(), model: emb.model.trim(), dims: Number(emb.dims.trim()), apiKey })}
  />
</div>
