<script lang="ts">
  // A state-changing action behind a confirm dialog: POST, announce the
  // result as a toast, then let the page refetch.
  import { sendJSON } from '../api.svelte';
  import { describe } from '../manage';
  import { toast } from '../toast.svelte';
  import type { Accepted } from '../types';
  import Dialog from './Dialog.svelte';

  interface Props {
    label: string;
    title: string;
    body: string;
    path: string;
    danger?: boolean;
    // done names the result, e.g. "Re-run queued".
    done: string;
    ondone?: () => void;
  }
  let { label, title, body, path, danger = false, done, ondone }: Props = $props();
  let open = $state(false);
  let busy = $state(false);

  async function run(): Promise<void> {
    busy = true;
    try {
      const res = await sendJSON<Accepted>('POST', path);
      toast(res?.jobId ? `${done}, job #${res.jobId}` : done);
    } catch (err) {
      toast(`${label} failed: ${describe(err)}`, 'danger');
    } finally {
      busy = false;
      open = false;
      ondone?.();
    }
  }
</script>

<button class="btn" class:btn-danger={danger} onclick={() => (open = true)}>{label}</button>
<Dialog bind:open {title}>
  <p>{body}</p>
  {#snippet footer()}
    <button class="btn" onclick={() => (open = false)}>Keep as is</button>
    <button class="btn btn-primary" class:btn-danger={danger} onclick={run} disabled={busy}>{busy ? 'Working…' : label}</button>
  {/snippet}
</Dialog>
