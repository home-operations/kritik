<script lang="ts">
  // One headline number: its label, value, and change against the window
  // before, coloured by whether that change is good rather than by its sign.
  import { change } from '../format';

  let {
    label,
    value,
    sub = '',
    define = '',
    now,
    before,
    good,
    points = false,
  }: {
    label: string;
    value: string;
    sub?: string;
    // define says what the number counts.
    define?: string;
    now?: number | null;
    before?: number | null;
    // good is the direction that is good news; unset, a change is neither.
    good?: 'up' | 'down';
    // points compares two percentages by their difference.
    points?: boolean;
  } = $props();

  const delta = $derived(now === undefined || before === undefined ? undefined : change(now, before, points));
  const tone = $derived(!delta || delta.dir === 0 || !good ? 'muted' : (delta.dir > 0) === (good === 'up') ? 'ok' : 'danger');
</script>

<div class="stat" title={define || undefined}>
  <span class="stat-label">{label}</span>
  <span class="stat-line">
    <span class="stat-value">{value}</span>
    {#if delta}<span class="delta tone-{tone}" title="Against the window before">{delta.text}</span>{/if}
  </span>
  {#if sub}<span class="stat-sub">{sub}</span>{/if}
</div>
