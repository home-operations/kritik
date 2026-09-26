<script lang="ts">
  import Icon from '../Icon.svelte';
  import { mdiContentCopy, mdiCheck } from '../icons';
  let { text, label = 'Copy' }: { text: string; label?: string } = $props();
  let done = $state(false);

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      done = true;
      setTimeout(() => (done = false), 1500);
    } catch (err) {
      console.error('copy:', err);
    }
  }
</script>

<button class="btn btn-small" onclick={copy} title={label} aria-label={label}>
  <Icon path={done ? mdiCheck : mdiContentCopy} size={13} />
  <span>{done ? 'Copied' : 'Copy'}</span>
</button>
