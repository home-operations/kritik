<script lang="ts">
  // The 👍 and 👎 a finding's inline comment drew on GitHub; nothing when
  // it drew none.
  import Icon from '../Icon.svelte';
  import { mdiThumbUpOutline, mdiThumbDownOutline } from '../icons';
  let { up, down }: { up: number; down: number } = $props();
</script>

{#if up || down}
  <span class="reactions" title="Reactions to its comment on GitHub">
    {#if up}<span><Icon path={mdiThumbUpOutline} size={12} label="thumbs up" />{up}</span>{/if}
    {#if down}<span><Icon path={mdiThumbDownOutline} size={12} label="thumbs down" />{down}</span>{/if}
  </span>
{/if}
