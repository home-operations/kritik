<script lang="ts">
  import type { RepositoryDraft } from '../../spec';

  interface Props {
    repo: RepositoryDraft;
    index: number;
    operator: boolean;
    inv: (path: string) => boolean;
    onremove: () => void;
  }
  let { repo = $bindable(), index, operator, inv, onremove }: Props = $props();
  const p = $derived(`repositories[${index}]`);
  const opHint = 'operator only';
</script>

<div class="item-card">
  <div class="item-head">
    <span class="mono">{repo.name || 'New repository'}</span>
    <button type="button" class="btn btn-small btn-danger" onclick={onremove}>Remove repository</button>
  </div>
  <div class="fields">
    <label class="field">
      <span>Name (owner/repo)</span>
      <input data-path="{p}.name" aria-invalid={inv(`${p}.name`) || undefined} bind:value={repo.name} required />
    </label>
    <label class="field">
      <span>Enabled</span>
      <select data-path="{p}.enabled" aria-invalid={inv(`${p}.enabled`) || undefined} bind:value={repo.enabled}>
        <option value="">default</option>
        <option value="true">yes</option>
        <option value="false">no</option>
      </select>
    </label>
    <label class="field">
      <span>Filter</span>
      <input class="mono" data-path="{p}.filter" aria-invalid={inv(`${p}.filter`) || undefined} bind:value={repo.filter} />
    </label>
    <label class="field">
      <span>Settle</span>
      <input data-path="{p}.settle" aria-invalid={inv(`${p}.settle`) || undefined} bind:value={repo.settle} placeholder="e.g. 2m" />
    </label>
    <label class="field">
      <span>Ignore globs (one per line)</span>
      <textarea rows="3" data-path="{p}.ignore" aria-invalid={inv(`${p}.ignore`) || undefined} bind:value={repo.ignore}></textarea>
    </label>
    <label class="field">
      <span>Review instructions (one per line)</span>
      <textarea rows="3" data-path="{p}.review.instructions" aria-invalid={inv(`${p}.review.instructions`) || undefined} bind:value={repo.instructions}></textarea>
    </label>
    <label class="field field-check">
      <input type="checkbox" data-path="{p}.review.requireSuggestedFix" bind:checked={repo.requireSuggestedFix} />
      <span>Require a suggested fix</span>
    </label>
  </div>
  <div class="fields">
    <label class="field">
      <span>Mode {#if !operator}<span class="field-hint">({opHint})</span>{/if}</span>
      <select data-path="{p}.mode" aria-invalid={inv(`${p}.mode`) || undefined} bind:value={repo.mode} disabled={!operator}>
        <option value="">default</option>
        <option value="single">single</option>
        <option value="agentic">agentic</option>
      </select>
    </label>
    <label class="field">
      <span>Incremental: max delta files {#if !operator}<span class="field-hint">({opHint})</span>{/if}</span>
      <input inputmode="numeric" data-path="{p}.incremental" aria-invalid={inv(`${p}.incremental`) || undefined} bind:value={repo.maxDeltaFiles} disabled={!operator} />
    </label>
    <label class="field">
      <span>Agent (JSON) {#if !operator}<span class="field-hint">({opHint})</span>{/if}</span>
      <textarea rows="3" data-path="{p}.agent" aria-invalid={inv(`${p}.agent`) || undefined} bind:value={repo.agent} disabled={!operator}></textarea>
    </label>
  </div>
</div>
