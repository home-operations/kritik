<script lang="ts">
  import type { Tone } from '../format';
  let { tone = 'muted', label, title }: { tone?: Tone; label: string; title?: string } = $props();
</script>

<span class="pill tone-{tone}" {title}>{label}</span>
