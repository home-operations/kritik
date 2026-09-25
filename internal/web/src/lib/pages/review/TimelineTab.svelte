<script lang="ts">
  import type { ReviewDetail, RunnerRun } from '../../types';
  import { between, duration, tokens, usd, wholeNumber, bytes } from '../../format';
  import { absolute, clock } from '../../time.svelte';
  import Time from '../../components/Time.svelte';
  import BarChart from '../../components/BarChart.svelte';

  let { d }: { d: ReviewDetail } = $props();

  interface Segment {
    name: string;
    from: string;
    to: string | null;
    ms: number;
    cls: string;
  }

  function segments(r: RunnerRun, now: number): Segment[] {
    const out: Segment[] = [];
    const add = (name: string, cls: string, from: string | null, to: string | null) => {
      if (!from) return;
      out.push({ name, cls, from, to, ms: between(from, to ?? new Date(now).toISOString()) ?? 0 });
    };
    add('queued', 'seg-queued', r.createdAt, r.scheduledAt ?? r.startedAt ?? r.finishedAt);
    add('starting', 'seg-starting', r.scheduledAt, r.startedAt ?? r.finishedAt);
    add('running', 'seg-running', r.startedAt, r.finishedAt);
    return out;
  }

  const segs = $derived(d.runnerRun ? segments(d.runnerRun, clock.now) : []);
  const totalMs = $derived(segs.reduce((a, s) => a + s.ms, 0));
  const steps = $derived(
    (d.agentRun?.timeline ?? []).map((s) => ({
      label: String(s.index),
      value: s.inputTokens + s.outputTokens,
      title: `step ${s.index}: ${wholeNumber(s.inputTokens)} in, ${wholeNumber(s.outputTokens)} out, ${duration(s.durationMs)}, ${bytes(s.outputBytes)} tool output${s.tools.length ? `, ${s.tools.join(', ')}` : ''}`,
    })),
  );
</script>

{#if d.runnerRun}
  {@const r = d.runnerRun}
  <section class="panel" aria-labelledby="tl-runner">
    <header class="panel-head">
      <h2 id="tl-runner">Runner</h2>
      <span class="small muted">{r.phase}{totalMs ? ` · ${duration(totalMs)} total` : ''}</span>
    </header>
    {#if segs.length}
      <div class="phase-bar" role="img" aria-label={segs.map((s) => `${s.name} ${duration(s.ms)}`).join(', ')}>
        {#each segs as s (s.name)}
          <span class="phase-seg {s.cls}" style:flex-grow={Math.max(s.ms, 1)} title="{s.name}: {duration(s.ms)}">{s.name}</span>
        {/each}
      </div>
    {/if}
    <div class="table-wrap">
      <table class="data">
        <thead><tr><th scope="col">Phase</th><th scope="col">From</th><th scope="col">To</th><th scope="col">Took</th></tr></thead>
        <tbody>
          {#each segs as s (s.name)}
            <tr><td>{s.name}</td><td title={absolute(s.from)}><Time iso={s.from} /></td><td><Time iso={s.to} /></td><td>{s.to ? duration(s.ms) : `${duration(s.ms)} so far`}</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
    <dl class="deflist">
      <dt>Job</dt><dd class="mono">{r.jobName || '—'}</dd>
      <dt>Pod</dt><dd class="mono">{r.podName || '—'}</dd>
      <dt>Node</dt><dd class="mono">{r.nodeName || '—'}</dd>
      <dt>Heartbeat</dt><dd><Time iso={r.heartbeatAt} /></dd>
      <dt>Exit code</dt><dd>{r.exitCode ?? '—'}</dd>
      <dt>Termination</dt><dd>{r.terminationReason || '—'}{r.deadlineExceeded ? ' (deadline exceeded)' : ''}</dd>
      {#if r.error}<dt>Error</dt><dd class="error-text">{r.error}</dd>{/if}
    </dl>
  </section>
{:else}
  <p class="state-msg">No runner was used for this review.</p>
{/if}

{#if d.agentRun}
  {@const a = d.agentRun}
  <section class="panel" aria-labelledby="tl-agent">
    <header class="panel-head">
      <h2 id="tl-agent">Agent steps</h2>
      <span class="small muted">{a.steps} steps · {a.stopReason} · {usd(a.costUsd)}</span>
    </header>
    {#if steps.length}
      <BarChart bars={steps} label="Tokens per agent step" format={tokens} />
    {/if}
    <dl class="deflist">
      <dt>Model</dt><dd class="mono">{a.model}</dd>
      <dt>Tokens</dt><dd>{tokens(a.usage.input)} in · {tokens(a.usage.cacheRead)} cache read · {tokens(a.usage.cacheWrite)} cache write · {tokens(a.usage.output)} out</dd>
      <dt>Tool calls</dt><dd class="mono">{Object.entries(a.toolCalls).map(([k, v]) => `${k}×${v}`).join(', ') || '—'}</dd>
      {#if a.sources.length}
        <dt>Sources</dt>
        <dd>
          <ul class="plain-list">
            {#each a.sources as s, i (i)}<li class="mono small">{s}</li>{/each}
          </ul>
        </dd>
      {/if}
      {#if a.error}<dt>Error</dt><dd class="error-text">{a.error}</dd>{/if}
    </dl>
  </section>
{/if}
