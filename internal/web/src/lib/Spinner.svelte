<script lang="ts">
  // A simple rotating-arc loading indicator. Inherits the surrounding text
  // colour via currentColor, so it tints muted in lists and accent at full size.
  let { size = 18 }: { size?: number } = $props();
</script>

<svg
  class="spinner"
  width={size}
  height={size}
  viewBox="0 0 32 32"
  fill="none"
  aria-hidden="true"
>
  <circle cx="16" cy="16" r="13" stroke="currentColor" stroke-width="2.5" opacity="0.25" />
  <path
    d="M29 16a13 13 0 0 0-13-13"
    stroke="currentColor"
    stroke-width="2.5"
    stroke-linecap="round"
  />
</svg>
