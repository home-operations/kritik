<script lang="ts">
  import { duration } from '../../format';
  import { inheritsHint } from '../../manage';
  import type { RepositoryDraft } from '../../spec';
  import type { ConfigSource, RepoSettings } from '../../types';

  interface Props {
    repo: RepositoryDraft;
    index: number;
    // inherited is what the entry's fields take from the account when left
    // empty; none when creating.
    inherited?: { settings: RepoSettings; sources: Record<string, ConfigSource> };
    inv: (path: string) => boolean;
    onremove: () => void;
  }
  let { repo = $bindable(), index, inherited, inv, onremove }: Props = $props();
  const own = $derived(inherited?.settings);
  const hint = (key: string, value: string, fallback = '') => (inherited ? inheritsHint(value, inherited.sources[key]) : fallback);
  const list = (xs: string[] | undefined) => (xs?.length ? xs.join(', ') : 'none');
  const p = $derived(`repositories[${index}]`);
</script>

<div class="item-card">
  <div class="item-head">
    <span class="mono">{repo.name || 'New repository'}</span>
    <button type="button" class="btn btn-small btn-danger" onclick={onremove}>Remove repository</button>
  </div>
  <div class="fields">
    <label class="field">
      <span>Name (without the owner)</span>
      <input data-path="{p}.name" aria-invalid={inv(`${p}.name`) || undefined} bind:value={repo.name} required />
    </label>
    <label class="field">
      <span>Enabled</span>
      <select data-path="{p}.enabled" aria-invalid={inv(`${p}.enabled`) || undefined} bind:value={repo.enabled}>
        <option value="">{own ? `default: ${own.enabled ? 'yes' : 'no'}` : 'default'}</option>
        <option value="true">yes</option>
        <option value="false">no</option>
      </select>
    </label>
    <label class="field">
      <span>Filter</span>
      <input class="mono" data-path="{p}.filter" aria-invalid={inv(`${p}.filter`) || undefined} bind:value={repo.filter} placeholder={hint('filter', own?.filter || 'no filter')} />
    </label>
    <label class="field">
      <span>Settle</span>
      <input data-path="{p}.settle" aria-invalid={inv(`${p}.settle`) || undefined} bind:value={repo.settle} placeholder={hint('settle', duration((own?.settleSeconds ?? 0) * 1000) || '0s', 'e.g. 2m')} />
    </label>
    <label class="field">
      <span>Ignore globs (one per line)</span>
      <textarea rows="3" data-path="{p}.ignore" aria-invalid={inv(`${p}.ignore`) || undefined} bind:value={repo.ignore} placeholder={own ? `adds to ${list(own.ignore)}` : ''}></textarea>
    </label>
    <label class="field">
      <span>Review instructions (one per line)</span>
      <textarea rows="3" data-path="{p}.review.instructions" aria-invalid={inv(`${p}.review.instructions`) || undefined} bind:value={repo.instructions} placeholder={hint('review.instructions', list(own?.review.instructions))}></textarea>
    </label>
    <label class="field field-check">
      <input type="checkbox" data-path="{p}.review.requireSuggestedFix" bind:checked={repo.requireSuggestedFix} />
      <span>Require a suggested fix</span>
    </label>
  </div>
  <div class="fields">
    <label class="field">
      <span>Mode</span>
      <select data-path="{p}.mode" aria-invalid={inv(`${p}.mode`) || undefined} bind:value={repo.mode}>
        <option value="">{own ? `default: ${own.mode}` : 'default'}</option>
        <option value="single">single</option>
        <option value="agentic">agentic</option>
      </select>
    </label>
    <label class="field">
      <span>Incremental: max delta files</span>
      <input inputmode="numeric" data-path="{p}.incremental" aria-invalid={inv(`${p}.incremental`) || undefined} bind:value={repo.maxDeltaFiles} placeholder={hint('incremental.maxDeltaFiles', String(own?.maxDeltaFiles))} />
    </label>
    <label class="field">
      <span>Agent (JSON)</span>
      <textarea rows="3" data-path="{p}.agent" aria-invalid={inv(`${p}.agent`) || undefined} bind:value={repo.agent}></textarea>
    </label>
  </div>
</div>
