<script lang="ts">
  import { untrack } from 'svelte';
  import type { DiffFile } from '../../diff';
  import type { Finding } from '../../types';
  import Icon from '../../Icon.svelte';
  import { mdiChevronDown, mdiChevronRight } from '../../icons';
  import FindingCard from './FindingCard.svelte';

  let { file, findings, initiallyOpen = true }: { file: DiffFile; findings: Finding[]; initiallyOpen?: boolean } = $props();
  let open = $state(untrack(() => initiallyOpen));
  const id = `df${Math.random().toString(36).slice(2, 9)}`;

  // Findings keyed by the new-side line they point at; anything that points
  // outside the hunks shown is listed under the file header instead.
  const byLine = $derived.by(() => {
    const m = new Map<number, Finding[]>();
    for (const f of findings) m.set(f.line, [...(m.get(f.line) ?? []), f]);
    return m;
  });
  const shownLines = $derived(new Set(file.lines.map((l) => l.newNo).filter((n) => n !== null)));
  const orphans = $derived(findings.filter((f) => !shownLines.has(f.line)));
</script>

<section class="diff-file">
  <h3 class="diff-file-head">
    <button aria-expanded={open} aria-controls={open ? id : undefined} onclick={() => (open = !open)}>
      <Icon path={open ? mdiChevronDown : mdiChevronRight} size={14} />
      <span class="mono diff-path">{file.oldPath && file.oldPath !== file.newPath && file.newPath ? `${file.oldPath} → ${file.newPath}` : file.path}</span>
      <span class="small"><span class="add">+{file.added}</span> <span class="del">−{file.removed}</span></span>
      {#if !open && !initiallyOpen}<span class="small muted">{file.lines.length} lines, collapsed</span>{/if}
      {#if findings.length}<span class="badge">{findings.length} finding{findings.length === 1 ? '' : 's'}</span>{/if}
    </button>
  </h3>
  {#if open}
    <div {id}>
      {#each orphans as f (f.id)}<div class="diff-finding"><FindingCard {f} /></div>{/each}
      {#if file.lines.length === 0}
        <p class="small muted diff-empty">{file.header.slice(1).join(' · ') || 'No textual changes.'}</p>
      {:else}
        <div class="table-wrap">
          <table class="diff">
            <tbody>
              {#each file.lines as l, i (i)}
                <tr class="dl dl-{l.kind}">
                  <td class="ln" aria-hidden="true">{l.oldNo ?? ''}</td>
                  <td class="ln" aria-hidden="true">{l.newNo ?? ''}</td>
                  <td class="code"><span class="sign" aria-hidden="true">{l.kind === 'add' ? '+' : l.kind === 'del' ? '-' : ' '}</span>{l.text}</td>
                </tr>
                {#if l.newNo !== null && l.kind !== 'del' && byLine.has(l.newNo)}
                  <tr class="dl-finding">
                    <td colspan="3">
                      {#each byLine.get(l.newNo) ?? [] as f (f.id)}<div class="diff-finding"><FindingCard {f} compact /></div>{/each}
                    </td>
                  </tr>
                {/if}
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>
  {/if}
</section>
