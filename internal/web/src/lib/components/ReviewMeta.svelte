<script lang="ts">
  // The one-line facts about a review run: trigger, mode, scope, model,
  // cost, tokens, duration. Shared by the pull timeline and the review page.
  import type { Review } from '../types';
  import { duration, tokens, usd, wholeNumber, shortSha } from '../format';
  let { r }: { r: Review } = $props();
</script>

<span class="meta-line">
  <span title="Trigger">{r.trigger}</span>
  <span>{r.mode}</span>
  <span>{r.scope}</span>
  {#if r.model}<span class="mono">{r.model}</span>{/if}
  <span class="mono" title={r.headSha}>{shortSha(r.headSha)}</span>
  <span title="Cost">{usd(r.costUsd)}</span>
  <span title="{wholeNumber(r.tokens.input)} in / {wholeNumber(r.tokens.output)} out">
    {tokens(r.tokens.input)} in · {tokens(r.tokens.output)} out
  </span>
  {#if r.durationMs !== null}<span title="Duration">{duration(r.durationMs)}</span>{/if}
</span>
