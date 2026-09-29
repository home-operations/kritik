<script lang="ts">
  // The form for the instance spec: its connections, the instance's
  // provider keys and its embedder, and every other setting as JSON, in a
  // SpecForm, which does the rest.
  import type { Snippet } from 'svelte';
  import { buildInstanceSpec, hasTypedSecret, instanceDraftOf, newConnection, newEmbedding, newProvider, type InstanceDraft } from '../../spec';
  import SpecForm from './SpecForm.svelte';
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
  let { dirty = $bindable(false), ...form }: Props = $props();
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
      {#each draft.providers as prov, i (prov.key)}
        <ProviderFields
          bind:prov={draft.providers[i]!}
          {inv}
          onremove={() => structural(() => (draft.providers = draft.providers.filter((x) => x.key !== prov.key)))}
        />
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.providers.push(newProvider()))}>Add provider key</button></div>
    </fieldset>

    <fieldset id="instance-embedding">
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
  {/snippet}
</SpecForm>
