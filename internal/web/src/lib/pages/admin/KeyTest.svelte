<script lang="ts">
  // Tests a key before it is saved: one cheap call to the provider it is
  // for, whose answer is shown as it came.
  import { sendJSON } from '../../api.svelte';
  import { describe } from '../../manage';
  import type { SecretDraft } from '../../spec';
  import type { EmbeddingTestRequest, ProviderTestRequest, SecretInput, TestResult } from '../../types';

  interface Props {
    path: string;
    // request builds the test's body around the key, as keyInput gives it.
    request: (key: SecretInput) => ProviderTestRequest | EmbeddingTestRequest;
    secret: SecretDraft;
    keepable: boolean;
    // onmodels receives the models a provider lists.
    onmodels?: (models: string[]) => void;
  }
  let { path, request, secret, keepable, onmodels }: Props = $props();
  let busy = $state(false);
  let result = $state<{ ok: boolean; text: string } | undefined>(undefined);

  // keyInput is the key as the test API takes it, or why there is none.
  function keyInput(): SecretInput | string {
    if (secret.mode === 'keep' && keepable) return { keep: true };
    if (secret.mode === 'replace' && secret.value !== '') return { value: secret.value };
    return 'enter the key to test it';
  }

  async function test(): Promise<void> {
    const key = keyInput();
    if (typeof key === 'string') {
      result = { ok: false, text: key };
      return;
    }
    busy = true;
    result = undefined;
    try {
      const r = await sendJSON<TestResult>('POST', path, request(key));
      if (r.ok) {
        result = { ok: true, text: r.models?.length ? `The key works; ${r.models.length} models offered.` : 'The key works.' };
        if (r.models?.length) onmodels?.(r.models);
      } else {
        result = { ok: false, text: r.error ?? 'The test failed.' };
      }
    } catch (err) {
      result = { ok: false, text: describe(err) };
    } finally {
      busy = false;
    }
  }
</script>

<div class="key-test">
  <button type="button" class="btn btn-small" onclick={test} disabled={busy}>{busy ? 'Testing…' : 'Test key'}</button>
  {#if result}
    <span class="small" class:muted={result.ok} role="status" data-testid="key-test-result">{result.text}</span>
  {/if}
</div>
