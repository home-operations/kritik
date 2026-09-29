<script lang="ts">
  // The form for an account's entry in the instance spec, in a SpecForm,
  // which does the rest.
  import type { Snippet } from 'svelte';
  import { buildSpec, draftOf, hasTypedSecret, newProvider, newRepository, type AccountDraft } from '../../spec';
  import { duration } from '../../format';
  import { inheritsHint } from '../../manage';
  import type { Inherited } from '../../types';
  import SpecForm from './SpecForm.svelte';
  import ProviderFields from './ProviderFields.svelte';
  import RepositoryFields from './RepositoryFields.svelte';

  type Obj = Record<string, unknown>;

  interface Props {
    initial: Obj;
    // inherited is what fields left empty take.
    inherited: Inherited;
    saving: boolean;
    errMessage?: string;
    errPath?: string;
    errSeq?: number;
    alertAction?: Snippet;
    dirty?: boolean;
    onsave: (spec: Obj) => void | Promise<void>;
  }
  let { inherited, dirty = $bindable(false), ...form }: Props = $props();

  const own = $derived(inherited.account);
  const hint = (key: string, value: string) => inheritsHint(value, inherited.accountSources[key]);
</script>

<SpecForm {...form} bind:dirty {draftOf} build={buildSpec} {hasTypedSecret}>
  {#snippet jsonNotice()}
    The spec as JSON. Secrets read as <span class="mono">{'{"keep": true}'}</span>; give a new one as
    <span class="mono">{'{"value": "…"}'}</span>. Values typed into the form are not carried over.
  {/snippet}
  {#snippet fields(draft: AccountDraft, inv: (path: string) => boolean, structural: (edit: () => void) => void)}
    <fieldset>
      <legend>Account <span class="mono">{draft.forge}/{draft.name}</span></legend>
      <div class="fields">
        <label class="field">
          <span>Review model</span>
          <input class="mono" data-path="models.review" aria-invalid={inv('models.review') || undefined} bind:value={draft.reviewModel} placeholder={hint('models.review', own.models.review || 'no model')} />
        </label>
        <label class="field">
          <span>Fallback model</span>
          <input class="mono" data-path="models.fallback" aria-invalid={inv('models.fallback') || undefined} bind:value={draft.fallbackModel} placeholder={hint('models.fallback', own.models.fallback || 'no fallback')} />
        </label>
        <label class="field">
          <span>Filter</span>
          <input class="mono" data-path="filter" aria-invalid={inv('filter') || undefined} bind:value={draft.filter} placeholder={hint('filter', own.filter || 'no filter')} />
        </label>
        <label class="field">
          <span>Forks</span>
          <select data-path="forks" aria-invalid={inv('forks') || undefined} bind:value={draft.forks}>
            <option value="">{own ? `default: ${own.forks ? 'review' : 'skip'}` : 'default'}</option>
            <option value="true">review</option>
            <option value="false">skip</option>
          </select>
        </label>
        <label class="field">
          <span>Settle</span>
          <input data-path="settle" aria-invalid={inv('settle') || undefined} bind:value={draft.settle} placeholder={hint('settle', duration((own.settleSeconds ?? 0) * 1000) || '0s')} />
        </label>
      </div>
    </fieldset>

    <fieldset>
      <legend>Limits</legend>
      <div class="fields">
        <label class="field">
          <span>Concurrency</span>
          <input inputmode="numeric" data-path="limits.concurrency" aria-invalid={inv('limits.concurrency') || undefined} bind:value={draft.concurrency} placeholder={hint('limits', String(own.limits.concurrency))} />
        </label>
        <label class="field">
          <span>Reviews per day</span>
          <input inputmode="numeric" data-path="limits.reviewsPerDay" aria-invalid={inv('limits.reviewsPerDay') || undefined} bind:value={draft.reviewsPerDay} placeholder={hint('limits', own.limits.reviewsPerDay ? String(own.limits.reviewsPerDay) : 'unlimited')} />
        </label>
        <label class="field">
          <span>Tokens per month</span>
          <input inputmode="numeric" data-path="limits.tokensPerMonth" aria-invalid={inv('limits.tokensPerMonth') || undefined} bind:value={draft.tokensPerMonth} placeholder={hint('limits', own.limits.tokensPerMonth ? String(own.limits.tokensPerMonth) : 'unlimited')} />
        </label>
        <label class="field">
          <span>Runner (JSON)</span>
          <textarea rows="3" data-path="runner" aria-invalid={inv('runner') || undefined} bind:value={draft.runner}></textarea>
        </label>
      </div>
    </fieldset>

    <fieldset id="account-providers">
      <legend>Provider keys</legend>
      <p class="field-hint">
        The account's own model keys. A model named <span class="mono">&lt;key name&gt;/&lt;model&gt;</span> runs on its key, and the
        account pays for it.
      </p>
      {#each draft.providers as prov, i (prov.key)}
        <ProviderFields
          bind:prov={draft.providers[i]!}
          account={`${draft.forge}/${draft.name}`}
          {inv}
          onremove={() => structural(() => (draft.providers = draft.providers.filter((x) => x.key !== prov.key)))}
        />
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.providers.push(newProvider()))}>Add provider key</button></div>
    </fieldset>

    <fieldset id="account-repositories">
      <legend>Repositories</legend>
      {#each draft.repositories as repo, i (repo.key)}
        <RepositoryFields
          bind:repo={draft.repositories[i]!}
          index={i}
          inherited={{ settings: inherited.repository, sources: inherited.repositorySources }}
          {inv}
          onremove={() => structural(() => (draft.repositories = draft.repositories.filter((x) => x.key !== repo.key)))}
        />
      {:else}
        <p class="field-hint">No repositories listed.</p>
      {/each}
      <div><button type="button" class="btn" onclick={() => structural(() => draft.repositories.push(newRepository()))}>Add repository</button></div>
    </fieldset>
  {/snippet}
</SpecForm>
