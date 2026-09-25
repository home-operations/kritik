<script lang="ts">
  // A minimal vertical bar chart in inline SVG: one series, a labelled
  // y-axis maximum and baseline, x labels under each bar, and a <title>
  // tooltip per bar. The whole chart carries an accessible description.
  export interface Bar {
    label: string;
    value: number;
    title: string;
  }
  interface Props {
    bars: Bar[];
    label: string;
    format: (n: number) => string;
    height?: number;
  }
  let { bars, label, format, height = 160 }: Props = $props();

  const pad = { top: 12, right: 8, bottom: 26, left: 52 };
  const barW = 22;
  const gap = 8;
  const width = $derived(Math.max(240, pad.left + pad.right + bars.length * (barW + gap)));
  const max = $derived(Math.max(0, ...bars.map((b) => b.value)));
  const plotH = $derived(height - pad.top - pad.bottom);
  const y = (v: number) => pad.top + plotH - (max > 0 ? (v / max) * plotH : 0);
  // Label every nth bar so x labels never collide.
  const every = $derived(Math.max(1, Math.ceil(bars.length / 12)));
</script>

<div class="chart-wrap">
  <svg class="chart" {width} {height} role="img" aria-label={label} viewBox="0 0 {width} {height}">
    <line class="chart-axis" x1={pad.left} x2={width - pad.right} y1={y(0)} y2={y(0)} />
    <line class="chart-grid" x1={pad.left} x2={width - pad.right} y1={pad.top} y2={pad.top} />
    <text class="chart-tick" x={pad.left - 6} y={pad.top + 4} text-anchor="end">{format(max)}</text>
    <text class="chart-tick" x={pad.left - 6} y={y(0) + 4} text-anchor="end">{format(0)}</text>
    {#each bars as b, i (i)}
      {@const x = pad.left + gap / 2 + i * (barW + gap)}
      <g class="chart-bar">
        <title>{b.title}</title>
        <rect {x} y={y(b.value)} width={barW} height={Math.max(0, y(0) - y(b.value))} rx="2" />
        {#if i % every === 0}
          <text class="chart-tick" x={x + barW / 2} y={height - 8} text-anchor="middle">{b.label}</text>
        {/if}
      </g>
    {/each}
  </svg>
  <ul class="sr-only" aria-label="{label}, values">
    {#each bars as b, i (i)}<li>{b.title}</li>{/each}
  </ul>
</div>
