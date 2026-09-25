<script lang="ts">
  import type { ReviewStatus } from '../types';
  import { reviewTone } from '../format';
  import Pill from './Pill.svelte';
  let { status, title }: { status: ReviewStatus; title?: string } = $props();
</script>

<Pill tone={reviewTone[status]} label={status} {title} />
