<script lang="ts">
  // One write-only secret: keep what is stored, replace it with a new value,
  // generate one (webhook secrets), or leave it unset. The password input
  // starts empty every time it appears and is cleared when another choice
  // is picked, so a typed value never lingers behind a "keep".
  import type { SecretDraft, SecretMode } from '../../spec';

  interface Props {
    label: string;
    path: string;
    secret: SecretDraft;
    generatable?: boolean;
    optional?: boolean;
    invalid?: boolean;
    hint?: string;
  }
  let { label, path, secret = $bindable(), generatable = false, optional = false, invalid = false, hint }: Props = $props();
  const name = `s${Math.random().toString(36).slice(2, 9)}`;

  const modes = $derived(
    [
      secret.wasSet ? (['keep', 'Keep current'] as const) : undefined,
      ['replace', secret.wasSet ? 'Replace with a new value' : 'Enter a value'] as const,
      generatable ? (['generate', 'Generate'] as const) : undefined,
      optional ? (['none', secret.wasSet ? 'Remove' : 'Not set'] as const) : undefined,
    ].filter((m) => m !== undefined),
  );

  function pick(m: SecretMode): void {
    secret.mode = m;
    secret.value = '';
  }
</script>

<fieldset class="field" class:invalid data-path={path}>
  <legend>{label} <span class="field-hint">{secret.wasSet ? 'set' : 'not set'}</span>
    {#if invalid}<span class="field-error">needs attention</span>{/if}</legend>
  <div class="secret-modes">
    {#each modes as [m, text] (m)}
      <label>
        <input type="radio" {name} value={m} checked={secret.mode === m} onchange={() => pick(m)} />
        {text}
      </label>
    {/each}
  </div>
  {#if secret.mode === 'replace'}
    <input
      type="password"
      autocomplete="new-password"
      aria-label={`${label}: new value`}
      aria-invalid={invalid || undefined}
      bind:value={secret.value}
    />
  {/if}
  {#if hint}<span class="field-hint">{hint}</span>{/if}
</fieldset>
