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
  import SettingRow from '../../components/SettingRow.svelte';
  import Segmented from '../../components/Segmented.svelte';

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
    {@const thorough = (draft.thoroughness || own.review.thoroughness) === 'thorough'}
    {@const forks = draft.forks ? draft.forks === 'true' : own.forks}
    {@const on = draft.enabled ? draft.enabled === 'true' : own.enabled}
    <section class="setting-section" id="account-reviews" aria-labelledby="account-reviews-h">
      <h3 id="account-reviews-h">Reviews</h3>
      <p class="setting-section-sub">
        How kritik reviews <span class="mono">{draft.forge}/{draft.name}</span>'s pull requests, where a repository's own entry
        does not say otherwise. An empty field inherits what its placeholder names.
      </p>
      <div class="setting-rows">
        <SettingRow id="set-review-model" label="Review model" hint="What reviews and follow-ups run on, as <provider>/<model>.">
          <input class="mono" aria-labelledby="set-review-model" data-path="models.review" aria-invalid={inv('models.review') || undefined} bind:value={draft.reviewModel} placeholder={hint('models.review', own.models.review || 'no model')} />
        </SettingRow>
        <SettingRow id="set-fallback-model" label="Fallback model" hint="What the provider falls back to when the review model cannot answer; it must be on the same provider.">
          <input class="mono" aria-labelledby="set-fallback-model" data-path="models.fallback" aria-invalid={inv('models.fallback') || undefined} bind:value={draft.fallbackModel} placeholder={hint('models.fallback', own.models.fallback || 'no fallback')} />
        </SettingRow>
        <SettingRow
          id="set-thoroughness"
          label="Thoroughness"
          hint="What a review reports."
          note={thorough ? 'Comments on anything a maintainer could act on, nits and questions included.' : 'Comments only on what would stop the review.'}
        >
          <Segmented
            label="Thoroughness"
            path="review.thoroughness"
            options={[
              { value: '', label: `Default (${own.review.thoroughness})` },
              { value: 'thorough', label: 'Thorough' },
              { value: 'focused', label: 'Focused' },
            ]}
            value={draft.thoroughness}
            onchange={(v) => (draft.thoroughness = v)}
          />
        </SettingRow>
        <SettingRow
          id="set-forks"
          label="Forks"
          hint="Pull requests whose head is in another repository."
          note={forks ? 'Reviewed like any other pull request.' : 'Reviewed only when a maintainer comments "@<bot> review".'}
        >
          <Segmented
            label="Forks"
            path="forks"
            options={[
              { value: '', label: `Default (${own.forks ? 'review' : 'skip'})` },
              { value: 'true', label: 'Review' },
              { value: 'false', label: 'Skip' },
            ]}
            value={draft.forks}
            onchange={(v) => (draft.forks = v)}
          />
        </SettingRow>
        <SettingRow
          id="set-enabled"
          label="Repositories"
          hint="Where a repository without an entry of its own starts, including ones the App reaches later."
          note={on ? 'Reviewed and indexed unless turned off.' : 'Registered, but not reviewed or indexed until turned on.'}
        >
          <Segmented
            label="Repositories"
            path="enabled"
            options={[
              { value: '', label: `Default (${own.enabled ? 'on' : 'off'})` },
              { value: 'true', label: 'On' },
              { value: 'false', label: 'Off' },
            ]}
            value={draft.enabled}
            onchange={(v) => (draft.enabled = v)}
          />
        </SettingRow>
        <SettingRow id="set-filter" label="Filter" hint="A CEL expression over the pull request, such as !pr.draft; one it rejects is not reviewed.">
          <input class="mono" aria-labelledby="set-filter" data-path="filter" aria-invalid={inv('filter') || undefined} bind:value={draft.filter} placeholder={hint('filter', own.filter || 'no filter')} />
        </SettingRow>
        <SettingRow id="set-settle" label="Settle" hint="How long a new head waits before its review, so a burst of pushes is reviewed once.">
          <input aria-labelledby="set-settle" data-path="settle" aria-invalid={inv('settle') || undefined} bind:value={draft.settle} placeholder={hint('settle', duration((own.settleSeconds ?? 0) * 1000) || '0s')} />
        </SettingRow>
      </div>
    </section>

    <section class="setting-section" id="account-limits" aria-labelledby="account-limits-h">
      <h3 id="account-limits-h">Limits</h3>
      <p class="setting-section-sub">Caps on what the account spends. A review past a cap ends capped.</p>
      <div class="setting-rows">
        <SettingRow id="set-concurrency" label="Concurrency" hint="How many model calls may run at once, per model.">
          <input inputmode="numeric" aria-labelledby="set-concurrency" data-path="limits.concurrency" aria-invalid={inv('limits.concurrency') || undefined} bind:value={draft.concurrency} placeholder={hint('limits', String(own.limits.concurrency))} />
        </SettingRow>
        <SettingRow id="set-reviews-per-day" label="Reviews per day" hint="Review passes a calendar day.">
          <input inputmode="numeric" aria-labelledby="set-reviews-per-day" data-path="limits.reviewsPerDay" aria-invalid={inv('limits.reviewsPerDay') || undefined} bind:value={draft.reviewsPerDay} placeholder={hint('limits', own.limits.reviewsPerDay ? String(own.limits.reviewsPerDay) : 'unlimited')} />
        </SettingRow>
        <SettingRow id="set-tokens-per-month" label="Tokens per month" hint="Input and output tokens a calendar month.">
          <input inputmode="numeric" aria-labelledby="set-tokens-per-month" data-path="limits.tokensPerMonth" aria-invalid={inv('limits.tokensPerMonth') || undefined} bind:value={draft.tokensPerMonth} placeholder={hint('limits', own.limits.tokensPerMonth ? String(own.limits.tokensPerMonth) : 'unlimited')} />
        </SettingRow>
        <SettingRow id="set-runner" label="Runner" hint="The review runner's deadline and resources, as JSON.">
          <textarea rows="3" aria-labelledby="set-runner" data-path="runner" aria-invalid={inv('runner') || undefined} bind:value={draft.runner}></textarea>
        </SettingRow>
      </div>
    </section>

    <fieldset class="setting-section" id="account-providers">
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

    <fieldset class="setting-section" id="account-repositories">
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
