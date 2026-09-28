<script lang="ts">
  // The form for an account's entry in the instance spec. It edits a draft
  // (see spec.ts) and hands the built entry to onsave; the caller does the request and passes back any
  // error, whose path highlights and focuses the field it names. A field
  // editable says the caller may not change, by the server's policy table,
  // stays disabled (ADR-0010 §2.7).
  import { tick, untrack, type Snippet } from 'svelte';
  import {
    buildSpec,
    draftOf,
    hasTypedSecret,
    newProvider,
    newRepository,
    pathMatches,
    type SpecError,
    type AccountDraft,
  } from '../../spec';
  import { duration } from '../../format';
  import { inheritsHint } from '../../manage';
  import type { Inherited } from '../../types';
  import ProviderFields from './ProviderFields.svelte';
  import RepositoryFields from './RepositoryFields.svelte';

  type Obj = Record<string, unknown>;

  interface Props {
    initial: Obj;
    // editable says whether the caller may change the setting at a key.
    editable: (key: string) => boolean;
    // inherited is what fields left empty take.
    inherited?: Inherited;
    saving: boolean;
    // The last failed save's message and the spec path it points at.
    errMessage?: string;
    errPath?: string;
    // Bumped on every failed save, so a repeated error still refocuses.
    errSeq?: number;
    alertAction?: Snippet;
    dirty?: boolean;
    submitLabel?: string;
    onsave: (spec: Obj) => void | Promise<void>;
  }
  let {
    initial,
    editable,
    inherited,
    saving,
    errMessage = '',
    errPath = '',
    errSeq = 0,
    alertAction,
    dirty = $bindable(false),
    submitLabel = 'Save',
    onsave,
  }: Props = $props();

  let draft = $state<AccountDraft>(untrack(() => draftOf(initial)));
  const baseline = untrack(() => JSON.stringify(buildSpec(draftOf(initial)).spec));
  let clientError = $state<SpecError | undefined>(undefined);
  let jsonMode = $state(false);
  let jsonText = $state('');
  let jsonEntered = '';
  let formEl = $state<HTMLFormElement | undefined>(undefined);

  const formDirty = $derived(JSON.stringify(buildSpec(draft).spec) !== baseline);
  $effect(() => {
    dirty = formDirty || (jsonMode && jsonText !== jsonEntered);
  });

  // A structural edit shifts indexes, so a server error pointing at a path
  // is dismissed by one; clearedSeq is the errSeq dismissed.
  let clearedSeq = $state(-1);
  const serverShown = $derived(clearedSeq !== errSeq);
  const activePath = $derived(clientError ? clientError.path : serverShown ? errPath : '');
  const alert = $derived(
    clientError ? `${clientError.path ? `${clientError.path}: ` : ''}${clientError.message}` : serverShown ? errMessage : '',
  );

  function structural(edit: () => void): void {
    edit();
    clientError = undefined;
    if (errPath) clearedSeq = errSeq;
  }
  const inv = (path: string) => pathMatches(path, activePath);
  const opHint = 'admin only';
  const own = $derived(inherited?.account);
  const hint = (key: string, value: string, fallback = '') =>
    inherited ? inheritsHint(value, inherited.accountSources[key]) : fallback;

  function focusPath(path: string): void {
    if (!path || !formEl) return;
    for (const el of formEl.querySelectorAll<HTMLElement>('[data-path]')) {
      if (!pathMatches(el.dataset.path ?? '', path)) continue;
      const target = el.matches('input, select, textarea') ? el : el.querySelector<HTMLElement>('input, select, textarea');
      target?.focus();
      target?.scrollIntoView({ block: 'center' });
      return;
    }
  }

  // A new server error moves focus to the field it names.
  $effect(() => {
    void errSeq;
    const p = untrack(() => errPath);
    if (p && !untrack(() => jsonMode)) void tick().then(() => focusPath(p));
  });

  function parseJSON(): Obj | undefined {
    try {
      const v: unknown = JSON.parse(jsonText);
      if (typeof v === 'object' && v !== null && !Array.isArray(v)) return v as Obj;
      clientError = { path: '', message: 'the spec must be a JSON object' };
    } catch (err) {
      clientError = { path: '', message: `the JSON does not parse: ${err instanceof Error ? err.message : String(err)}` };
    }
    return undefined;
  }

  function toggleJSON(): void {
    clientError = undefined;
    if (!jsonMode) {
      // The JSON view never shows a typed secret, so switching would lose it.
      if (hasTypedSecret(draft)) {
        clientError = {
          path: '',
          message: 'Save, or clear, the secret values typed into the form first: the JSON view never shows them, so they would be lost.',
        };
        return;
      }
      jsonText = JSON.stringify(buildSpec(draft, true).spec, null, 2);
      jsonEntered = jsonText;
      jsonMode = true;
      return;
    }
    const v = parseJSON();
    if (!v) return;
    draft = draftOf(v);
    jsonMode = false;
    jsonText = '';
  }

  async function submit(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    clientError = undefined;
    if (jsonMode) {
      const v = parseJSON();
      if (v) await onsave(v);
      return;
    }
    const b = buildSpec(draft);
    if (b.error) {
      clientError = b.error;
      await tick();
      focusPath(b.error.path);
      return;
    }
    await onsave(b.spec);
  }
</script>

<form class="form" bind:this={formEl} onsubmit={submit} novalidate>
  <div class="form-actions">
    <button type="button" class="btn" aria-pressed={jsonMode} onclick={toggleJSON}>
      {jsonMode ? 'Back to the form' : 'Advanced: edit JSON'}
    </button>
  </div>

  {#if jsonMode}
    <p class="notice">
      The spec as JSON. Secrets read as <span class="mono">{'{"keep": true}'}</span>; give a new one as
      <span class="mono">{'{"value": "…"}'}</span> or, for a webhook secret, <span class="mono">{'{"generate": true}'}</span>. Values
      typed into the form are not carried over.
    </p>
    <label class="field">
      <span>Spec JSON</span>
      <textarea class="json-edit" spellcheck="false" bind:value={jsonText} aria-invalid={!!clientError || undefined}></textarea>
    </label>
  {:else}
    <fieldset>
      <legend>Account <span class="mono">{draft.forge}/{draft.name}</span></legend>
      <div class="fields">
        <label class="field">
          <span>Review model {#if !editable('models.review')}<span class="field-hint">({opHint})</span>{/if}</span>
          <input class="mono" data-path="models.review" aria-invalid={inv('models.review') || undefined} bind:value={draft.reviewModel} placeholder={hint('models.review', own?.models.review || 'no model', 'provider/model')} disabled={!editable('models.review')} />
        </label>
        <label class="field">
          <span>Fallback model {#if !editable('models.fallback')}<span class="field-hint">({opHint})</span>{/if}</span>
          <input class="mono" data-path="models.fallback" aria-invalid={inv('models.fallback') || undefined} bind:value={draft.fallbackModel} placeholder={hint('models.fallback', own?.models.fallback || 'no fallback', 'provider/model')} disabled={!editable('models.fallback')} />
        </label>
        <label class="field">
          <span>Filter</span>
          <input class="mono" data-path="filter" aria-invalid={inv('filter') || undefined} bind:value={draft.filter} placeholder={hint('filter', own?.filter || 'no filter')} />
        </label>
        <label class="field">
          <span>Forks {#if !editable('forks')}<span class="field-hint">({opHint})</span>{/if}</span>
          <select data-path="forks" aria-invalid={inv('forks') || undefined} bind:value={draft.forks} disabled={!editable('forks')}>
            <option value="">{own ? `default: ${own.forks ? 'review' : 'skip'}` : 'default'}</option>
            <option value="true">review</option>
            <option value="false">skip</option>
          </select>
        </label>
        <label class="field">
          <span>Settle</span>
          <input data-path="settle" aria-invalid={inv('settle') || undefined} bind:value={draft.settle} placeholder={hint('settle', duration((own?.settleSeconds ?? 0) * 1000) || '0s', 'e.g. 2m')} />
        </label>
      </div>
    </fieldset>

    <fieldset>
      <legend>Limits {#if !editable('limits')}<span class="field-hint">({opHint})</span>{/if}</legend>
      <div class="fields">
        <label class="field">
          <span>Concurrency</span>
          <input inputmode="numeric" data-path="limits.concurrency" aria-invalid={inv('limits.concurrency') || undefined} bind:value={draft.concurrency} placeholder={hint('limits', String(own?.limits.concurrency))} disabled={!editable('limits')} />
        </label>
        <label class="field">
          <span>Reviews per day</span>
          <input inputmode="numeric" data-path="limits.reviewsPerDay" aria-invalid={inv('limits.reviewsPerDay') || undefined} bind:value={draft.reviewsPerDay} placeholder={hint('limits', own?.limits.reviewsPerDay ? String(own.limits.reviewsPerDay) : 'unlimited')} disabled={!editable('limits')} />
        </label>
        <label class="field">
          <span>Tokens per month</span>
          <input inputmode="numeric" data-path="limits.tokensPerMonth" aria-invalid={inv('limits.tokensPerMonth') || undefined} bind:value={draft.tokensPerMonth} placeholder={hint('limits', own?.limits.tokensPerMonth ? String(own.limits.tokensPerMonth) : 'unlimited')} disabled={!editable('limits')} />
        </label>
        <label class="field">
          <span>Runner (JSON) {#if !editable('runner')}<span class="field-hint">({opHint})</span>{/if}</span>
          <textarea rows="3" data-path="runner" aria-invalid={inv('runner') || undefined} bind:value={draft.runner} disabled={!editable('runner')}></textarea>
        </label>
      </div>
    </fieldset>

    <fieldset>
      <legend>Provider keys</legend>
      <p class="field-hint">
        The account's own model keys. A model named <span class="mono">&lt;key name&gt;/&lt;model&gt;</span> runs on its key, and the
        account pays for it.
      </p>
      {#each draft.providers as prov, i (prov.key)}
        <ProviderFields
          bind:prov={draft.providers[i]!}
          {inv}
          onremove={() => structural(() => (draft.providers = draft.providers.filter((x) => x.key !== prov.key)))}
        />
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.providers.push(newProvider()))}>Add provider key</button></div>
    </fieldset>

    <fieldset>
      <legend>Repositories</legend>
      {#each draft.repositories as repo, i (repo.key)}
        <RepositoryFields
          bind:repo={draft.repositories[i]!}
          index={i}
          {editable}
          inherited={inherited && { settings: inherited.repository, sources: inherited.repositorySources }}
          {inv}
          onremove={() => structural(() => (draft.repositories = draft.repositories.filter((x) => x.key !== repo.key)))}
        />
      {:else}
        <p class="field-hint">No repositories listed.</p>
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.repositories.push(newRepository()))}>Add repository</button></div>
    </fieldset>
  {/if}

  <div aria-live="assertive">
    {#if alert}
      <div class="form-alert" role="alert">
        <span>{alert}</span>
        {#if alertAction && !clientError}{@render alertAction()}{/if}
      </div>
    {/if}
  </div>

  <div class="form-actions">
    <button type="submit" class="btn btn-primary" disabled={saving}>{saving ? 'Saving…' : submitLabel}</button>
    {#if dirty}<span class="field-hint">Unsaved changes</span>{/if}
  </div>
</form>
