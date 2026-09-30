<script lang="ts">
  // The form for the instance spec: the instance's review defaults,
  // provider keys, embedder and connections, and every other setting as
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
  import SettingRow from '../../components/SettingRow.svelte';
  import Segmented from '../../components/Segmented.svelte';

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
  const own = $derived(inherited.defaults);

  // withSource is note, what a default's current choice does, saying where
  // an inherited value comes from when that is not kritik's own.
  function withSource(key: keyof InstanceInherited['defaults'], set: string, note: string): string {
    const src = own[key].source;
    return set || src === 'default' ? note : `${note} Set by ${sourceName(src)}.`;
  }

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
    {@const mode = draft.mode || own.mode.value}
    {@const thorough = (draft.thoroughness || own['review.thoroughness'].value) === 'thorough'}
    {@const forks = (draft.forks || own.forks.value) === 'true'}
    <section class="setting-section" id="instance-defaults" aria-labelledby="instance-defaults-h">
      <h3 id="instance-defaults-h">Review defaults</h3>
      <p class="setting-section-sub">
        How every account and repository reviews unless its own settings say otherwise. An empty field inherits what its
        placeholder names.
      </p>
      <div class="setting-rows">
        <SettingRow id="set-default-review-model" label="Review model" hint="What reviews and follow-ups run on, as <key name>/<model>.">
          <input
            class="mono"
            aria-labelledby="set-default-review-model"
            data-path="defaults.models.review"
            aria-invalid={inv('defaults.models.review') || undefined}
            bind:value={draft.reviewModel}
            placeholder={inherited.review ? inheritsHint(inherited.review.value, inherited.review.source) : 'no model'}
          />
        </SettingRow>
        <SettingRow
          id="set-default-fallback-model"
          label="Fallback model"
          hint="What the provider falls back to when the review model cannot answer; it must be on the same key."
        >
          <input
            class="mono"
            aria-labelledby="set-default-fallback-model"
            data-path="defaults.models.fallback"
            aria-invalid={inv('defaults.models.fallback') || undefined}
            bind:value={draft.fallbackModel}
            placeholder={inherited.fallback ? inheritsHint(inherited.fallback.value, inherited.fallback.source) : 'no fallback'}
          />
        </SettingRow>
        <SettingRow
          id="set-default-mode"
          label="Mode"
          hint="How a review reads the change."
          note={withSource(
            'mode',
            draft.mode,
            mode === 'agentic'
              ? 'The model reads the repository with tools in a runner before it reports; it runs longer and costs more.'
              : 'One model call over the diff and the context kritik gathers for it.',
          )}
        >
          <Segmented
            label="Mode"
            path="defaults.mode"
            options={[
              { value: '', label: `Default (${own.mode.value})` },
              { value: 'single', label: 'Single' },
              { value: 'agentic', label: 'Agentic' },
            ]}
            value={draft.mode}
            onchange={(v) => (draft.mode = v)}
          />
        </SettingRow>
        <SettingRow
          id="set-default-thoroughness"
          label="Thoroughness"
          hint="What a review reports."
          note={withSource(
            'review.thoroughness',
            draft.thoroughness,
            thorough ? 'Comments on anything a maintainer could act on, nits and questions included.' : 'Comments only on what would stop the review.',
          )}
        >
          <Segmented
            label="Thoroughness"
            path="defaults.review.thoroughness"
            options={[
              { value: '', label: `Default (${own['review.thoroughness'].value})` },
              { value: 'thorough', label: 'Thorough' },
              { value: 'focused', label: 'Focused' },
            ]}
            value={draft.thoroughness}
            onchange={(v) => (draft.thoroughness = v)}
          />
        </SettingRow>
        <SettingRow
          id="set-default-forks"
          label="Forks"
          hint="Pull requests whose head is in another repository."
          note={withSource('forks', draft.forks, forks ? 'Reviewed like any other pull request.' : 'Reviewed only when a maintainer comments "@<bot> review".')}
        >
          <Segmented
            label="Forks"
            path="defaults.forks"
            options={[
              { value: '', label: `Default (${own.forks.value === 'true' ? 'review' : 'skip'})` },
              { value: 'true', label: 'Review' },
              { value: 'false', label: 'Skip' },
            ]}
            value={draft.forks}
            onchange={(v) => (draft.forks = v)}
          />
        </SettingRow>
        <SettingRow id="set-default-settle" label="Settle" hint="How long a new head waits before its review, so a burst of pushes is reviewed once.">
          <input
            aria-labelledby="set-default-settle"
            data-path="defaults.settle"
            aria-invalid={inv('defaults.settle') || undefined}
            bind:value={draft.settle}
            placeholder={inheritsHint(own.settle.value, own.settle.source)}
          />
        </SettingRow>
      </div>
    </section>

    <section class="setting-section" id="instance-providers" aria-labelledby="instance-providers-h">
      <h3 id="instance-providers-h">Provider keys</h3>
      <p class="setting-section-sub">
        The instance's model keys. A model named <span class="mono">&lt;key name&gt;/&lt;model&gt;</span> runs on its key; an
        account's own keys, set on its Configuration page, come first.
      </p>
      {#if fileProviders.length}
        <ul class="setting-rows" aria-label="Provider keys the configuration file sets">
          {#each fileProviders as [name, p] (name)}
            <li class="setting-row">
              <div class="setting-text">
                <span class="setting-name mono">{name}</span>
                <span class="setting-hint">
                  {p.type}{#if p.baseUrl} at <span class="mono">{p.baseUrl}</span>{/if}, from {sourceName(p.source)}
                </span>
              </div>
              <div class="setting-control">
                {#if draft.providers.some((x) => x.name.trim() === name)}
                  <span class="small muted">Overridden by the key below</span>
                {:else}
                  <button type="button" class="btn btn-small" aria-label="Override {name}" onclick={() => structural(() => override(draft, name))}>Override</button>
                {/if}
              </div>
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
    </section>

    <section class="setting-section" id="instance-embedding" aria-labelledby="instance-embedding-h">
      <h3 id="instance-embedding-h">Embeddings</h3>
      <p class="setting-section-sub">
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
      {:else}
        <div class="setting-rows">
          <SettingRow
            id="set-embedder"
            label="Embedder"
            hint={fileEmb
              ? `Uses ${fileEmb.model} (${fileEmb.dims} dimensions) at ${fileEmb.baseUrl}, from ${sourceName(fileEmb.source)}.`
              : 'None: reviews run without similar code.'}
          >
            {#if fileEmb}
              <button type="button" class="btn btn-small" onclick={() => structural(() => overrideEmbedding(draft))}>Override</button>
            {:else}
              <button type="button" class="btn btn-small" onclick={() => structural(() => (draft.embedding = newEmbedding()))}>Add embedder</button>
            {/if}
          </SettingRow>
        </div>
      {/if}
    </section>

    <section class="setting-section" id="instance-connections" aria-labelledby="instance-connections-h">
      <h3 id="instance-connections-h">Connections</h3>
      <p class="setting-section-sub">
        Each GitHub App kritik serves accounts through. Connections the configuration file declares are listed under Instance
        settings and change there.
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
    </section>
  {/snippet}
</SpecForm>
