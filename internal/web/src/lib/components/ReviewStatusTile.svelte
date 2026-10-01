<script lang="ts">
  // A review's status as an icon on a tinted tile and a word: colour where
  // the status asks for attention, quiet where it does not.
  import type { ReviewStatus } from '../types';
  import { reviewTone } from '../format';
  import Icon from '../Icon.svelte';
  import { mdiCheck, mdiProgressClock, mdiAlertCircleOutline, mdiGaugeFull, mdiDebugStepOver, mdiSwapHorizontal, mdiCancel } from '../icons';

  let { status, title }: { status: ReviewStatus; title?: string } = $props();

  const icons: Record<ReviewStatus, string> = {
    running: mdiProgressClock,
    prepared: mdiProgressClock,
    completed: mdiCheck,
    superseded: mdiSwapHorizontal,
    skipped: mdiDebugStepOver,
    capped: mdiGaugeFull,
    failed: mdiAlertCircleOutline,
    canceled: mdiCancel,
  };
</script>

<span class="status tone-{reviewTone[status]}" {title}>
  <span class="status-tile"><Icon path={icons[status]} size={12} /></span>
  <span class="status-word">{status}</span>
</span>
