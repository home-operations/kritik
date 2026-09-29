<script lang="ts" generics="V extends string">
  // A choice of a few, all in view: a radio group drawn as one control.
  // Arrow keys move the choice, as in a native radio group.
  let {
    label,
    options,
    value,
    onchange,
    disabled = false,
  }: {
    label: string;
    options: readonly { value: V; label: string }[];
    value: V;
    onchange: (v: V) => void;
    disabled?: boolean;
  } = $props();

  let group = $state<HTMLDivElement | undefined>(undefined);

  function onkeydown(e: KeyboardEvent): void {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const i = options.findIndex((o) => o.value === value);
    const next = options[(i + step + options.length) % options.length]!;
    onchange(next.value);
    group?.querySelectorAll<HTMLButtonElement>('[role="radio"]')[options.indexOf(next)]?.focus();
  }
</script>

<div class="segmented" role="radiogroup" aria-label={label} bind:this={group}>
  {#each options as o (o.value)}
    <button
      type="button"
      role="radio"
      aria-checked={o.value === value}
      tabindex={o.value === value ? 0 : -1}
      {disabled}
      onclick={() => onchange(o.value)}
      {onkeydown}
    >
      {o.label}
    </button>
  {/each}
</div>
