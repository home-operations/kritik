<script lang="ts">
  // The form for the instance spec: its connections, the instance's
  // provider keys, default models and embedder, and every other setting as
  // JSON, in a SpecForm, which does the rest. What the configuration file
  // sets of those shows as what the spec overrides.
  import type { Snippet } from 'svelte';
  import { buildInstanceSpec, hasTypedSecret, instanceDraftOf, newConnection, newEmbedding, newProvider, type InstanceDraft } from '../../spec';
  import { inheritsHint, sourceName } from '../../manage';
  import type { InstanceInherited } from '../../types';
  import SpecForm from './SpecForm.svelte';
  import ConnectionFields from './ConnectionFields.svelte';
  import EmbeddingFields from './EmbeddingFields.svelte';
  import ProviderFields from './ProviderFields.svelte';

  type Obj = Record<string, unknown>;

  interface Props {
    initial: Obj;
    // inherited is what the configuration file sets, which the spec
    // overrides.
    inherited: InstanceInherited;
    saving: boolean;
    errMessage?: string;
    errPath?: string;
    errSeq?: number;
    alertAction?: Snippet;
    dirty?: boolean;
    onsave: (spec: Obj) => void | Promise<void>;
  }
  let { inherited, dirty = $bindable(false), ...form }: Props = $props();

  const fileProviders = $derived(Object.entries(inherited.providers).sort(([a], [b]) => a.localeCompare(b)));
  const fileEmb = $derived(inherited.embedding);

  // override starts the spec's own provider under a name the file uses,
  // from the file's type and endpoint; its key is entered anew.
  function override(draft: InstanceDraft, name: string): void {
    const p = inherited.providers[name]!;
    draft.providers.push({ ...newProvider(), name, type: p.type, baseUrl: p.baseUrl });
  }

  // overrideEmbedding starts the spec's own embedder from the file's.
  function overrideEmbedding(draft: InstanceDraft): void {
    const e = inherited.embedding!;
    draft.embedding = { ...newEmbedding(), baseUrl: e.baseUrl, model: e.model, dims: String(e.dims) };
  }
</script>

<SpecForm {...form} bind:dirty draftOf={instanceDraftOf} build={buildInstanceSpec} {hasTypedSecret}>
  {#snippet jsonNotice()}
    The whole instance spec as JSON: defaults, polling, indexing, retention, egress, tools and accounts too. Secrets read as
    <span class="mono">{'{"keep": true}'}</span>; give a new one as <span class="mono">{'{"value": "…"}'}</span> or, for a
    webhook secret, <span class="mono">{'{"generate": true}'}</span>.
  {/snippet}
  {#snippet fields(draft: InstanceDraft, inv: (path: string) => boolean, structural: (edit: () => void) => void)}
    <fieldset id="instance-connections">
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

    <fieldset id="instance-providers">
      <legend>Provider keys</legend>
      <p class="field-hint">
        The instance's model keys. A model named <span class="mono">&lt;key name&gt;/&lt;model&gt;</span> runs on its key; an
        account's own keys, set on its admin page, come first.
      </p>
      {#if fileProviders.length}
        <ul class="setup-list" aria-label="Provider keys the configuration file sets">
          {#each fileProviders as [name, p] (name)}
            <li>
              <span class="mono">{name}</span>: {p.type}{#if p.baseUrl} at <span class="mono">{p.baseUrl}</span>{/if}, from {sourceName(p.source)}
              {#if draft.providers.some((x) => x.name.trim() === name)}
                <span class="small muted">(overridden by the key below)</span>
              {:else}
                <button type="button" class="btn btn-small" onclick={() => structural(() => override(draft, name))}>Override</button>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
      {#each draft.providers as prov, i (prov.key)}
        <ProviderFields
          bind:prov={draft.providers[i]!}
          {inv}
          onremove={() => structural(() => (draft.providers = draft.providers.filter((x) => x.key !== prov.key)))}
        />
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.providers.push(newProvider()))}>Add provider key</button></div>
    </fieldset>

    <fieldset id="instance-models">
      <legend>Default models</legend>
      <p class="field-hint">
        The models every account and repository reviews with unless it names its own, as
        <span class="mono">&lt;key name&gt;/&lt;model&gt;</span>.
      </p>
      <div class="fields">
        <label class="field">
          <span>Review model</span>
          <input
            class="mono"
            data-path="defaults.models.review"
            aria-invalid={inv('defaults.models.review') || undefined}
            bind:value={draft.reviewModel}
            placeholder={inherited.review ? inheritsHint(inherited.review.value, inherited.review.source) : 'no model'}
          />
        </label>
        <label class="field">
          <span>Fallback model</span>
          <input
            class="mono"
            data-path="defaults.models.fallback"
            aria-invalid={inv('defaults.models.fallback') || undefined}
            bind:value={draft.fallbackModel}
            placeholder={inherited.fallback ? inheritsHint(inherited.fallback.value, inherited.fallback.source) : 'no fallback'}
          />
        </label>
      </div>
    </fieldset>

    <fieldset id="instance-embedding">
      <legend>Embeddings</legend>
      <p class="field-hint">
        The embedder that builds each repository's similar-code index, which reviews draw context from. Without one, reviews
        run without it.
      </p>
      {#if draft.embedding}
        {#if fileEmb}
          <p class="field-hint">
            Overrides <span class="mono">{fileEmb.model}</span> ({fileEmb.dims} dimensions) from {sourceName(fileEmb.source)}; remove
            it to use that one.
          </p>
        {/if}
        <EmbeddingFields bind:emb={draft.embedding} {inv} onremove={() => structural(() => (draft.embedding = undefined))} />
      {:else if fileEmb}
        <p class="field-hint">
          Uses <span class="mono">{fileEmb.model}</span> ({fileEmb.dims} dimensions) at <span class="mono">{fileEmb.baseUrl}</span>, from
          {sourceName(fileEmb.source)}.
        </p>
        <div><button type="button" class="btn" onclick={() => structural(() => overrideEmbedding(draft))}>Override</button></div>
      {:else}
        <div><button type="button" class="btn" onclick={() => structural(() => (draft.embedding = newEmbedding()))}>Add embedder</button></div>
      {/if}
    </fieldset>
  {/snippet}
</SpecForm>
