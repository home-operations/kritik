<script lang="ts">
  // The form for the instance spec: its connections, the instance's
  // provider keys and its embedder, and every other setting as JSON. It edits a draft (see
  // spec.ts) and hands the built spec to onsave; the caller does the
  // request and passes back any error, whose path highlights the field it
  // names.
  import { tick, untrack, type Snippet } from 'svelte';
  import {
    buildInstanceSpec,
    hasTypedSecret,
    instanceDraftOf,
    newConnection,
    newEmbedding,
    newProvider,
    pathMatches,
    type InstanceDraft,
    type SpecError,
  } from '../../spec';
  import ConnectionFields from './ConnectionFields.svelte';
  import EmbeddingFields from './EmbeddingFields.svelte';
  import ProviderFields from './ProviderFields.svelte';

  type Obj = Record<string, unknown>;

  interface Props {
    initial: Obj;
    saving: boolean;
    errMessage?: string;
    errPath?: string;
    errSeq?: number;
    alertAction?: Snippet;
    dirty?: boolean;
    onsave: (spec: Obj) => void | Promise<void>;
  }
  let { initial, saving, errMessage = '', errPath = '', errSeq = 0, alertAction, dirty = $bindable(false), onsave }: Props = $props();

  let draft = $state<InstanceDraft>(untrack(() => instanceDraftOf(initial)));
  const baseline = untrack(() => JSON.stringify(buildInstanceSpec(instanceDraftOf(initial)).spec));
  let clientError = $state<SpecError | undefined>(undefined);
  let jsonMode = $state(false);
  let jsonText = $state('');
  let jsonEntered = '';
  let formEl = $state<HTMLFormElement | undefined>(undefined);

  $effect(() => {
    dirty = JSON.stringify(buildInstanceSpec(draft).spec) !== baseline || (jsonMode && jsonText !== jsonEntered);
  });

  let clearedSeq = $state(-1);
  const serverShown = $derived(clearedSeq !== errSeq);
  const activePath = $derived(clientError ? clientError.path : serverShown ? errPath : '');
  const alert = $derived(
    clientError ? `${clientError.path ? `${clientError.path}: ` : ''}${clientError.message}` : serverShown ? errMessage : '',
  );
  const inv = (path: string) => pathMatches(path, activePath);

  function structural(edit: () => void): void {
    edit();
    clientError = undefined;
    if (errPath) clearedSeq = errSeq;
  }

  function focusPath(path: string): void {
    if (!path || !formEl) return;
    for (const el of formEl.querySelectorAll<HTMLElement>('[data-path]')) {
      if (!pathMatches(el.dataset.path ?? '', path)) continue;
      const target = el.matches('input, select, textarea') ? el : el.querySelector<HTMLElement>('input, select, textarea');
      target?.focus();
      return;
    }
  }

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
      if (hasTypedSecret(draft)) {
        clientError = {
          path: '',
          message: 'Save, or clear, the secret values typed into the form first: the JSON view never shows them, so they would be lost.',
        };
        return;
      }
      jsonText = JSON.stringify(buildInstanceSpec(draft, true).spec, null, 2);
      jsonEntered = jsonText;
      jsonMode = true;
      return;
    }
    const v = parseJSON();
    if (!v) return;
    draft = instanceDraftOf(v);
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
    const b = buildInstanceSpec(draft);
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
      The whole instance spec as JSON: defaults, polling, indexing, retention, egress, tools and accounts too. Secrets read as
      <span class="mono">{'{"keep": true}'}</span>; give a new one as <span class="mono">{'{"value": "…"}'}</span> or, for a
      webhook secret, <span class="mono">{'{"generate": true}'}</span>.
    </p>
    <label class="field">
      <span>Spec JSON</span>
      <textarea class="json-edit" spellcheck="false" bind:value={jsonText} aria-invalid={!!clientError || undefined}></textarea>
    </label>
  {:else}
    <fieldset>
      <legend>Connections</legend>
      <p class="field-hint">
        Each GitHub App kritik serves accounts through. Connections the configuration file declares are listed in the settings
        below and change there.
      </p>
      {#each draft.connections as inst, i (inst.key)}
        <ConnectionFields
          bind:inst={draft.connections[i]!}
          index={i}
          {inv}
          onremove={() => structural(() => (draft.connections = draft.connections.filter((x) => x.key !== inst.key)))}
        />
      {:else}
        <p class="field-hint">No connections in the dashboard.</p>
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.connections.push(newConnection()))}>Add connection</button></div>
    </fieldset>

    <fieldset>
      <legend>Provider keys</legend>
      <p class="field-hint">
        The instance's model keys. A model named <span class="mono">&lt;key name&gt;/&lt;model&gt;</span> runs on its key; an
        account's own keys, set on its admin page, come first.
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
      <legend>Embeddings</legend>
      <p class="field-hint">
        The embedder that builds each repository's similar-code index, which reviews draw context from. Without one, reviews
        run without it.
      </p>
      {#if draft.embedding}
        <EmbeddingFields bind:emb={draft.embedding} {inv} onremove={() => structural(() => (draft.embedding = undefined))} />
      {:else}
        <div><button type="button" class="btn" onclick={() => structural(() => (draft.embedding = newEmbedding()))}>Add embedder</button></div>
      {/if}
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
    <button type="submit" class="btn btn-primary" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
    {#if dirty}<span class="field-hint">Unsaved changes</span>{/if}
  </div>
</form>
