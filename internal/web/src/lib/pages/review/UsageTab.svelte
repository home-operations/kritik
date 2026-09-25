<script lang="ts">
  import type { ReviewDetail } from '../../types';
  import { tokens, usd, wholeNumber } from '../../format';
  import Time from '../../components/Time.svelte';

  let { d }: { d: ReviewDetail } = $props();
  const total = $derived(
    d.usage.reduce((a, u) => ({ in: a.in + u.inputTokens, out: a.out + u.outputTokens, cost: a.cost + u.costUsd }), { in: 0, out: 0, cost: 0 }),
  );
</script>

{#if d.usage.length === 0}
  <p class="state-msg">No usage recorded.</p>
{:else}
  <div class="table-wrap">
    <table class="data">
      <thead>
        <tr>
          <th scope="col">Role</th><th scope="col">Model</th><th scope="col">Upstream</th>
          <th scope="col" class="num">Input</th><th scope="col" class="num">Output</th><th scope="col" class="num">Cost</th><th scope="col">When</th>
        </tr>
      </thead>
      <tbody>
        {#each d.usage as u, i (i)}
          <tr>
            <td>{u.role}</td>
            <td class="mono small">{u.model}</td>
            <td class="mono small">{u.upstream}</td>
            <td class="num" title={wholeNumber(u.inputTokens)}>{tokens(u.inputTokens)}</td>
            <td class="num" title={wholeNumber(u.outputTokens)}>{tokens(u.outputTokens)}</td>
            <td class="num">{usd(u.costUsd)}</td>
            <td><Time iso={u.createdAt} /></td>
          </tr>
        {/each}
      </tbody>
      <tfoot>
        <tr>
          <th scope="row" colspan="3">Total</th>
          <td class="num" title={wholeNumber(total.in)}>{tokens(total.in)}</td>
          <td class="num" title={wholeNumber(total.out)}>{tokens(total.out)}</td>
          <td class="num">{usd(total.cost)}</td>
          <td></td>
        </tr>
      </tfoot>
    </table>
  </div>
{/if}
