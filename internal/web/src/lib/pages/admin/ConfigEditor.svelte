<script lang="ts">
  // The tenant spec form. It edits a draft (see spec.ts) and hands the
  // built spec to onsave; the caller does the request and passes back any
  // error, whose path highlights and focuses the field it names. Runner,
  // limits and a repository's mode, incremental and agent stay disabled
  // for anyone but an operator (ADR-0009 §2.15).
  import { tick, untrack, type Snippet } from 'svelte';
  import {
    buildSpec,
    draftOf,
    newInstallation,
    newRepository,
    pathMatches,
    type SpecError,
    type TenantDraft,
  } from '../../spec';
  import InstallationFields from './InstallationFields.svelte';
  import RepositoryFields from './RepositoryFields.svelte';

  type Obj = Record<string, unknown>;

  interface Props {
    initial: Obj;
    creating?: boolean;
    operator: boolean;
    saving: boolean;
    // The last failed save's message and the spec path it points at.
    errMessage?: string;
    errPath?: string;
    alertAction?: Snippet;
    dirty?: boolean;
    submitLabel?: string;
    onsave: (spec: Obj) => void | Promise<void>;
  }
  let {
    initial,
    creating = false,
    operator,
    saving,
    errMessage = '',
    errPath = '',
    alertAction,
    dirty = $bindable(false),
    submitLabel = 'Save',
    onsave,
  }: Props = $props();

  let draft = $state<TenantDraft>(untrack(() => draftOf(initial)));
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

  const activePath = $derived(clientError ? clientError.path : errPath);
  const alert = $derived(clientError ? `${clientError.path ? `${clientError.path}: ` : ''}${clientError.message}` : errMessage);
  const inv = (path: string) => pathMatches(path, activePath);
  const opHint = 'operator only';

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
    const p = errPath;
    if (p && !jsonMode) void tick().then(() => focusPath(p));
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
      <legend>Tenant</legend>
      <div class="fields">
        {#if creating}
          <label class="field">
            <span>Slug</span>
            <input class="mono" data-path="slug" aria-invalid={inv('slug') || undefined} bind:value={draft.slug} required />
          </label>
        {/if}
        <label class="field">
          <span>Review model</span>
          <input class="mono" data-path="models.review" aria-invalid={inv('models.review') || undefined} bind:value={draft.reviewModel} placeholder="provider/model" />
        </label>
        <label class="field">
          <span>Fallback model</span>
          <input class="mono" data-path="models.fallback" aria-invalid={inv('models.fallback') || undefined} bind:value={draft.fallbackModel} placeholder="provider/model" />
        </label>
        <label class="field">
          <span>Filter</span>
          <input class="mono" data-path="filter" aria-invalid={inv('filter') || undefined} bind:value={draft.filter} />
        </label>
        <label class="field">
          <span>Forks</span>
          <select data-path="forks" aria-invalid={inv('forks') || undefined} bind:value={draft.forks}>
            <option value="">default</option>
            <option value="true">review</option>
            <option value="false">skip</option>
          </select>
        </label>
        <label class="field">
          <span>Settle</span>
          <input data-path="settle" aria-invalid={inv('settle') || undefined} bind:value={draft.settle} placeholder="e.g. 2m" />
        </label>
      </div>
    </fieldset>

    <fieldset>
      <legend>Limits {#if !operator}<span class="field-hint">({opHint})</span>{/if}</legend>
      <div class="fields">
        <label class="field">
          <span>Concurrency</span>
          <input inputmode="numeric" data-path="limits.concurrency" aria-invalid={inv('limits.concurrency') || undefined} bind:value={draft.concurrency} disabled={!operator} />
        </label>
        <label class="field">
          <span>Reviews per day</span>
          <input inputmode="numeric" data-path="limits.reviewsPerDay" aria-invalid={inv('limits.reviewsPerDay') || undefined} bind:value={draft.reviewsPerDay} disabled={!operator} />
        </label>
        <label class="field">
          <span>Tokens per month</span>
          <input inputmode="numeric" data-path="limits.tokensPerMonth" aria-invalid={inv('limits.tokensPerMonth') || undefined} bind:value={draft.tokensPerMonth} disabled={!operator} />
        </label>
        <label class="field">
          <span>Runner (JSON) {#if !operator}<span class="field-hint">({opHint})</span>{/if}</span>
          <textarea rows="3" data-path="runner" aria-invalid={inv('runner') || undefined} bind:value={draft.runner} disabled={!operator}></textarea>
        </label>
      </div>
    </fieldset>

    <fieldset>
      <legend>Installations</legend>
      {#each draft.installations as inst, i (inst.key)}
        <InstallationFields
          bind:inst={draft.installations[i]!}
          index={i}
          {inv}
          onremove={() => (draft.installations = draft.installations.filter((x) => x.key !== inst.key))}
        />
      {:else}
        <p class="field-hint">No installations yet.</p>
      {/each}
      <div><button type="button" class="btn" onclick={() => draft.installations.push(newInstallation())}>Add installation</button></div>
    </fieldset>

    <fieldset>
      <legend>Repositories</legend>
      {#each draft.repositories as repo, i (repo.key)}
        <RepositoryFields
          bind:repo={draft.repositories[i]!}
          index={i}
          {operator}
          {inv}
          onremove={() => (draft.repositories = draft.repositories.filter((x) => x.key !== repo.key))}
        />
      {:else}
        <p class="field-hint">No repositories listed.</p>
      {/each}
      <div><button type="button" class="btn" onclick={() => draft.repositories.push(newRepository())}>Add repository</button></div>
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
