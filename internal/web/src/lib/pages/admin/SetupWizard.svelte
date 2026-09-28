<script lang="ts">
  // The first-run setup wizard (ADR-0014 §2.6): a modal that walks the
  // instance spec's sections in the order they depend on each other. Each
  // step saves through the same API as the admin console, so closing it
  // halfway loses nothing, and it reopens at the first step not done.
  import { untrack } from 'svelte';
  import { getJSON, sendJSON } from '../../api.svelte';
  import { href } from '../../router.svelte';
  import { describe, errorPath } from '../../manage';
  import { SETUP_STEPS, firstStep, setFlag, setupFlags } from '../../setup.svelte';
  import { toast } from '../../toast.svelte';
  import {
    buildInstanceSpec,
    instanceDraftOf,
    newEmbedding,
    newProvider,
    pathMatches,
    type EmbeddingDraft,
    type InstanceDraft,
  } from '../../spec';
  import type {
    AccountRepositories,
    AppInstallation,
    ConfigWriteResult,
    Connection,
    InstanceConfig,
    RegisterResult,
    SetupStatus,
  } from '../../types';
  import Dialog from '../../components/Dialog.svelte';
  import AppManifestForm from './AppManifestForm.svelte';
  import ProviderFields from './ProviderFields.svelte';
  import EmbeddingFields from './EmbeddingFields.svelte';

  type Obj = Record<string, unknown>;

  interface Props {
    open: boolean;
    status: SetupStatus;
    // refresh reloads the status until done holds of it, as a save reaches
    // the running configuration a moment after it is written.
    refresh: (done?: (s: SetupStatus) => boolean) => Promise<void>;
  }
  let { open = $bindable(), status, refresh }: Props = $props();

  let step = $state(untrack(() => firstStep(status, setupFlags)));
  let busy = $state(false);
  let errMessage = $state('');
  let errPath = $state('');
  const inv = (path: string) => pathMatches(path, errPath);

  // Reopening resumes at the first step not done.
  $effect(() => {
    if (open) step = untrack(() => firstStep(status, setupFlags));
  });

  function go(to: number): void {
    errMessage = '';
    errPath = '';
    step = to;
  }

  // pass marks a step with nothing to save done, and moves to the next.
  function pass(flag: 'listener' | 'skipEmbedding' | 'repositories', to: number): void {
    setFlag(flag, true);
    go(to);
  }

  function fail(err: unknown): void {
    errMessage = describe(err);
    errPath = errorPath(err);
  }

  // saveSpec writes edit's changes to the stored instance spec.
  async function saveSpec(edit: (draft: InstanceDraft) => void, extra?: (spec: Obj) => void): Promise<boolean> {
    const cfg = await getJSON<InstanceConfig>('/api/v1/config');
    const draft = instanceDraftOf(cfg.spec);
    edit(draft);
    const b = buildInstanceSpec(draft);
    if (b.error) {
      errMessage = `${b.error.path}: ${b.error.message}`;
      errPath = b.error.path;
      return false;
    }
    extra?.(b.spec);
    const r = await sendJSON<ConfigWriteResult>('PUT', '/api/v1/config', { revision: cfg.revision, spec: b.spec });
    toast(`Saved: revision ${r.revision}`);
    return true;
  }

  // The GitHub App step: which served accounts each connection's App is
  // installed on, checked every few seconds until one is.
  let conns = $state<Connection[]>([]);
  let installs = $state<Record<string, AppInstallation[]>>({});
  async function checkInstalls(): Promise<void> {
    try {
      conns = await getJSON<Connection[]>('/api/v1/operator/connections');
      for (const c of conns) {
        installs[c.name] = await getJSON<AppInstallation[]>(`/api/v1/operator/connections/${encodeURIComponent(c.name)}/installations`);
      }
      if (Object.values(installs).some((list) => list.some((i) => i.served))) setFlag('installed', true);
    } catch (err) {
      fail(err);
    }
  }
  $effect(() => {
    if (!open || step !== 1 || status.connections.length === 0) return;
    void checkInstalls();
    const t = setInterval(() => void checkInstalls(), 5000);
    return () => clearInterval(t);
  });

  // The model provider step.
  let provDraft = $state<InstanceDraft | undefined>(undefined);
  let models = $state<string[]>([]);
  let modelsOf = $state('');
  let reviewModel = $state('');
  let fallbackModel = $state('');
  $effect(() => {
    if (!open || step !== 2 || status.reviewModel || provDraft) return;
    void getJSON<InstanceConfig>('/api/v1/config').then(
      (cfg) => {
        const d = instanceDraftOf(cfg.spec);
        if (d.providers.length === 0) d.providers.push(newProvider());
        provDraft = d;
      },
      (err: unknown) => fail(err),
    );
  });

  function listModels(provider: string, list: string[]): void {
    models = list;
    modelsOf = provider.trim();
  }

  async function saveModel(): Promise<void> {
    const d = provDraft;
    if (!d) return;
    if (!reviewModel.trim()) {
      errMessage = 'Choose the model reviews run on.';
      errPath = 'defaults.models.review';
      return;
    }
    busy = true;
    errMessage = '';
    errPath = '';
    try {
      const saved = await saveSpec(
        (draft) => (draft.providers = d.providers),
        (spec) => {
          const defaults = { ...((spec.defaults as Obj | undefined) ?? {}) };
          const m: Obj = { ...((defaults.models as Obj | undefined) ?? {}), review: reviewModel.trim() };
          if (fallbackModel.trim()) m.fallback = fallbackModel.trim();
          defaults.models = m;
          spec.defaults = defaults;
        },
      );
      if (saved) {
        await refresh((s) => s.reviewModel !== '');
        go(3);
      }
    } catch (err) {
      fail(err);
    } finally {
      busy = false;
    }
  }

  // The embeddings step.
  let emb = $state<EmbeddingDraft>(newEmbedding());
  async function saveEmbedding(): Promise<void> {
    busy = true;
    errMessage = '';
    errPath = '';
    try {
      if (await saveSpec((draft) => (draft.embedding = emb))) {
        await refresh((s) => s.embedding);
        go(4);
      }
    } catch (err) {
      fail(err);
    } finally {
      busy = false;
    }
  }

  // The repositories step.
  let reached = $state<Record<string, AccountRepositories[]>>({});
  $effect(() => {
    if (!open || step !== 4) return;
    void (async () => {
      try {
        for (const c of status.connections) {
          reached[c] = await getJSON<AccountRepositories[]>(`/api/v1/operator/connections/${encodeURIComponent(c)}/repositories`);
        }
      } catch (err) {
        fail(err);
      }
    })();
  });

  async function registerRepositories(): Promise<void> {
    busy = true;
    errMessage = '';
    try {
      let added = 0;
      for (const c of status.connections) {
        const r = await sendJSON<RegisterResult>('POST', `/api/v1/operator/connections/${encodeURIComponent(c)}/repositories`);
        added += r.added;
      }
      toast(added === 1 ? 'Registered 1 new repository' : `Registered ${added} new repositories`);
      setFlag('repositories', true);
      go(5);
    } catch (err) {
      fail(err);
    } finally {
      busy = false;
    }
  }

  function accountsOf(): string[] {
    return [...new Set(conns.flatMap((c) => c.accounts))];
  }
  $effect(() => {
    if (open && step === 5 && conns.length === 0) {
      void getJSON<Connection[]>('/api/v1/operator/connections').then((c) => (conns = c), fail);
    }
  });
</script>

<Dialog bind:open title="Set up kritik" wide onclose={() => setFlag('dismissed', true)}>
  <ol class="setup-steps" aria-label="Setup steps">
    {#each SETUP_STEPS as name, i (name)}
      <li class:current={i === step} aria-current={i === step ? 'step' : undefined}>{name}</li>
    {/each}
  </ol>

  {#if step === 0}
    <p>
      kritik is served at <span class="mono">{status.webUrl}</span>. GitHub delivers each connection's webhook to
      <span class="mono">{status.hooksUrl}&lt;connection&gt;</span>: route <span class="mono">/hooks</span> there to the
      webhook listener.
    </p>
    {#if status.fileConnections.length}
      <p>
        The configuration file declares {status.fileConnections.length === 1 ? 'the connection' : 'the connections'}
        <span class="mono">{status.fileConnections.join(', ')}</span>, which change there.
      </p>
    {/if}
  {:else if step === 1}
    {#if status.connections.length === 0}
      <p>Create a GitHub App for kritik. GitHub sends you back here to install it.</p>
      <AppManifestForm />
      <p class="field-hint">Or declare an App you already have in the configuration file, and reload.</p>
    {:else}
      <p>Install the App on the accounts it serves. This step moves on once GitHub reports an installation.</p>
      {#each conns as c (c.name)}
        <div class="item-card">
          <div class="item-head"><span class="mono">{c.name}</span></div>
          <ul class="setup-list">
            {#each c.accounts as a (a)}
              {@const inst = installs[c.name]?.find((i) => i.account.toLowerCase() === a.toLowerCase())}
              <li>
                <span class="mono">{a}</span>:
                {#if inst}installed{:else}<span class="muted">not installed yet</span>{/if}
              </li>
            {/each}
          </ul>
        </div>
      {/each}
      {#if !setupFlags.installed}
        <label class="field-hint">
          <input type="checkbox" onchange={(e) => setFlag('installed', e.currentTarget.checked)} /> I installed it, and GitHub has not
          reported it yet
        </label>
      {/if}
    {/if}
  {:else if step === 2}
    {#if status.reviewModel}
      <p>Reviews run on <span class="mono">{status.reviewModel}</span>. Change it, or add keys, in the admin console.</p>
    {:else if provDraft}
      <p>The instance's model key, which every account's reviews run on unless it brings its own.</p>
      {#each provDraft.providers as prov, i (prov.key)}
        <ProviderFields
          bind:prov={provDraft.providers[i]!}
          {inv}
          onremove={() => provDraft && (provDraft.providers = provDraft.providers.filter((x) => x.key !== prov.key))}
          onmodels={(m) => listModels(prov.name, m)}
        />
      {/each}
      <div class="fields">
        <label class="field">
          <span>Review model</span>
          <input class="mono" list="setup-models" data-path="defaults.models.review" aria-invalid={inv('defaults.models.review') || undefined} bind:value={reviewModel} placeholder="provider/model" />
          <span class="field-hint">A key's name, a slash, and one of its models; test the key to list them.</span>
        </label>
        <label class="field">
          <span>Fallback model</span>
          <input class="mono" list="setup-models" bind:value={fallbackModel} placeholder="optional" />
        </label>
      </div>
      <datalist id="setup-models">
        {#each models as m (m)}<option value={`${modelsOf}/${m}`}></option>{/each}
      </datalist>
    {/if}
  {:else if step === 3}
    {#if status.embedding}
      <p>An embedder is set. Change it in the admin console.</p>
    {:else}
      <p>
        Optional: the embedder builds each repository's similar-code index, which reviews draw context from. Without one,
        reviews run without it.
      </p>
      <EmbeddingFields bind:emb {inv} onremove={() => (emb = newEmbedding())} />
    {/if}
  {:else if step === 4}
    <p>
      Every repository the App reaches is reviewed. Register them now so kritik polls them, and indexes them with an
      embedder, before the first webhook arrives. Turn one off, or set its mode, limits or own key, on its account's admin
      page.
    </p>
    {#each Object.entries(reached) as [c, accounts] (c)}
      {#each accounts as a (a.account)}
        <div class="item-card">
          <div class="item-head"><span class="mono">{a.account}</span> <span class="small muted">through {c}</span></div>
          {#if !a.installed}
            <p class="muted">The App is not installed here.</p>
          {:else}
            <p>
              {a.repositories.length === 1 ? '1 repository' : `${a.repositories.length} repositories`}:
              <span class="mono small">{a.repositories.slice(0, 10).map((r) => r.name).join(', ')}{a.repositories.length > 10 ? ', …' : ''}</span>
            </p>
          {/if}
        </div>
      {/each}
    {/each}
  {:else}
    <p>kritik can review. A pull request opened, or pushed to, in a served repository is reviewed within moments.</p>
    <ul class="setup-list">
      {#each accountsOf() as a (a)}
        <li><a href={href({ name: 'queue', slug: `github/${a}` })} onclick={() => (open = false)}>github/{a}</a>: watch its queue</li>
      {/each}
      <li><a href={href({ name: 'operator' })} onclick={() => (open = false)}>Admin console</a>: every setting</li>
    </ul>
  {/if}

  <div aria-live="assertive">
    {#if errMessage}<div class="form-alert" role="alert"><span>{errMessage}</span></div>{/if}
  </div>

  {#snippet footer()}
    <button class="btn" onclick={() => (open = false)}>Close</button>
    {#if step > 0}<button class="btn" onclick={() => go(step - 1)}>Back</button>{/if}
    {#if step === 0}
      <button class="btn btn-primary" onclick={() => pass('listener', 1)}>Next</button>
    {:else if step === 1}
      <button class="btn btn-primary" disabled={status.connections.length === 0 || !setupFlags.installed} onclick={() => go(2)}>Next</button>
    {:else if step === 2}
      {#if status.reviewModel}
        <button class="btn btn-primary" onclick={() => go(3)}>Next</button>
      {:else}
        <button class="btn btn-primary" disabled={busy || !provDraft} onclick={saveModel}>{busy ? 'Saving…' : 'Save and continue'}</button>
      {/if}
    {:else if step === 3}
      {#if status.embedding}
        <button class="btn btn-primary" onclick={() => go(4)}>Next</button>
      {:else}
        <button class="btn" onclick={() => pass('skipEmbedding', 4)}>Skip</button>
        <button class="btn btn-primary" disabled={busy} onclick={saveEmbedding}>{busy ? 'Saving…' : 'Save and continue'}</button>
      {/if}
    {:else if step === 4}
      <button class="btn" onclick={() => pass('repositories', 5)}>Skip</button>
      <button class="btn btn-primary" disabled={busy} onclick={registerRepositories}>{busy ? 'Registering…' : 'Register and continue'}</button>
    {:else}
      <button class="btn btn-primary" onclick={() => (open = false)}>Finish</button>
    {/if}
  {/snippet}
</Dialog>
