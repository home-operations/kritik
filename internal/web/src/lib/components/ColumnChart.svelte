<script lang="ts">
  // Columns over time, one or several stacked series, in inline SVG that
  // fills its container's width. Pointer and arrow keys read one column at
  // a time; the table view carries every value without hovering.
  interface Series {
    label: string;
    // color is a CSS colour, normally a --chart-* token.
    color: string;
  }
  interface Row {
    key: string;
    // label names the column on the axis, and title in the readout.
    label: string;
    title: string;
    // values are the series' in order, bottom of the stack first.
    values: number[];
  }
  interface Props {
    label: string;
    series: Series[];
    rows: Row[];
    format: (n: number) => string;
    view?: 'chart' | 'table';
    height?: number;
  }
  let { label, series, rows, format, view = 'chart', height = 180 }: Props = $props();

  const pad = { top: 10, right: 6, bottom: 22, left: 46 };
  const GAP = 2;
  let width = $state(0);
  const plotW = $derived(Math.max(0, width - pad.left - pad.right));
  const plotH = $derived(height - pad.top - pad.bottom);
  const totals = $derived(rows.map((r) => r.values.reduce((a, b) => a + b, 0)));

  // niceMax rounds the tallest column up to 1, 2, 2.5 or 5 times a power of
  // ten, so the gridlines fall on clean numbers.
  function niceMax(v: number): number {
    if (v <= 0) return 1;
    const p = 10 ** Math.floor(Math.log10(v));
    return ([1, 2, 2.5, 5, 10].find((m) => m * p >= v) ?? 10) * p;
  }
  const max = $derived(niceMax(Math.max(0, ...totals)));
  const ticks = $derived([0, max / 2, max]);
  const y = (v: number) => pad.top + plotH - (v / max) * plotH;

  const band = $derived(rows.length ? plotW / rows.length : 0);
  const barW = $derived(Math.max(2, Math.min(24, band * 0.64)));
  const every = $derived(Math.max(1, Math.ceil(rows.length / Math.max(1, Math.floor(plotW / 48)))));

  // segments stacks a column's values from the baseline, leaving a 2px gap
  // under each segment that has another above it; only the top one gets a
  // rounded end.
  function segments(r: Row) {
    const out: { y: number; h: number; color: string; top: boolean }[] = [];
    let base = 0;
    const last = r.values.findLastIndex((v) => v > 0);
    r.values.forEach((v, i) => {
      if (v <= 0) return;
      const y0 = y(base);
      base += v;
      const y1 = y(base);
      const h = Math.max(0, y0 - y1 - (i === last ? 0 : GAP));
      out.push({ y: i === last ? y1 : y1 + GAP, h, color: series[i]!.color, top: i === last });
    });
    return out;
  }

  // roundedTop is a column with its top corners rounded and its base square.
  function roundedTop(x: number, top: number, w: number, h: number): string {
    const r = Math.min(4, w / 2, h);
    return `M${x},${top + h}V${top + r}Q${x},${top} ${x + r},${top}H${x + w - r}Q${x + w},${top} ${x + w},${top + r}V${top + h}Z`;
  }

  let active = $state(-1);
  function onpointermove(e: PointerEvent): void {
    const box = (e.currentTarget as SVGElement).getBoundingClientRect();
    const i = Math.floor((e.clientX - box.left - pad.left) / band);
    active = i >= 0 && i < rows.length ? i : -1;
  }
  function onkeydown(e: KeyboardEvent): void {
    if (!rows.length) return;
    if (e.key === 'ArrowRight') active = Math.min(rows.length - 1, active + 1);
    else if (e.key === 'ArrowLeft') active = active <= 0 ? 0 : active - 1;
    else if (e.key === 'Home') active = 0;
    else if (e.key === 'End') active = rows.length - 1;
    else if (e.key === 'Escape') active = -1;
    else return;
    e.preventDefault();
  }
  const activeRow = $derived(active >= 0 ? rows[active] : undefined);
  const tipLeft = $derived(active >= 0 ? pad.left + band * (active + 0.5) : 0);
</script>

{#if view === 'table'}
  <div class="table-wrap chart-table">
    <table class="data">
      <thead>
        <tr>
          <th scope="col">Date</th>
          {#each series as s (s.label)}<th scope="col" class="num">{s.label}</th>{/each}
          {#if series.length > 1}<th scope="col" class="num">Total</th>{/if}
        </tr>
      </thead>
      <tbody>
        {#each rows as r, i (r.key)}
          <tr>
            <td>{r.title}</td>
            {#each r.values as v, j (j)}<td class="num">{format(v)}</td>{/each}
            {#if series.length > 1}<td class="num">{format(totals[i]!)}</td>{/if}
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{:else}
  {#if series.length > 1}
    <ul class="chart-legend" aria-label="{label} legend">
      {#each series as s (s.label)}<li><span class="chart-swatch" style:background={s.color}></span>{s.label}</li>{/each}
    </ul>
  {/if}
  <div class="column-chart">
    <div bind:clientWidth={width}>
      {#if width > 0}
        <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
        <svg
          class="chart"
          {width}
          {height}
          viewBox="0 0 {width} {height}"
          role="img"
          aria-label="{label}. Arrow keys read one column at a time."
          tabindex="0"
          {onpointermove}
          onpointerleave={() => (active = -1)}
          {onkeydown}
          onblur={() => (active = -1)}
        >
          {#each ticks as t (t)}
            <line class="chart-grid" x1={pad.left} x2={width - pad.right} y1={y(t)} y2={y(t)} />
            <text class="chart-tick" x={pad.left - 6} y={y(t) + 3.5} text-anchor="end">{format(t)}</text>
          {/each}
          {#if active >= 0}
            <rect class="chart-hover" x={pad.left + band * active} y={pad.top} width={band} height={plotH} />
          {/if}
          {#each rows as r, i (r.key)}
            {@const x = pad.left + band * i + (band - barW) / 2}
            <g class="chart-col" class:dim={active >= 0 && active !== i}>
              {#each segments(r) as s, j (j)}
                {#if s.top}
                  <path d={roundedTop(x, s.y, barW, s.h)} fill={s.color} />
                {:else}
                  <rect {x} y={s.y} width={barW} height={s.h} fill={s.color} />
                {/if}
              {/each}
            </g>
            {#if i % every === 0}
              <text class="chart-tick" x={pad.left + band * (i + 0.5)} y={height - 6} text-anchor="middle">{r.label}</text>
            {/if}
          {/each}
        </svg>
      {/if}
    </div>
    {#if activeRow}
      <div class="chart-tip" style:left="{tipLeft + 12}px" class:flip={tipLeft > width * 0.6}>
        <p class="chart-tip-head">{activeRow.title}</p>
        {#each [...series.keys()].reverse() as j (j)}
          <p class="chart-tip-row">
            <span class="chart-key" style:background={series[j]!.color}></span>
            <strong>{format(activeRow.values[j]!)}</strong>
            <span class="muted">{series[j]!.label}</span>
          </p>
        {/each}
      </div>
    {/if}
    <p class="sr-only" aria-live="polite">
      {#if activeRow}{activeRow.title}: {series.map((s, j) => `${format(activeRow.values[j]!)} ${s.label}`).join(', ')}{/if}
    </p>
  </div>
{/if}
