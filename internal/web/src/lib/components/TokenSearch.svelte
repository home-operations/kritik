<script lang="ts">
  // A search box that also takes filter tokens and suggests them: a
  // combobox whose list opens while the box has focus and something
  // matches. It reports the parsed text once typing pauses, so typing
  // doesn't fire a request per keystroke.
  import { parseTokens, suggest, complete, type Parsed, type Suggestion, type TokenSpec } from '../tokensearch';

  let {
    id,
    label,
    placeholder,
    specs,
    text = $bindable(''),
    input = $bindable(),
    ready = true,
    onapply,
  }: {
    id: string;
    label: string;
    placeholder: string;
    specs: readonly TokenSpec[];
    text?: string;
    input?: HTMLInputElement;
    // ready is whether the specs' values have loaded, so a token they don't
    // list yet is not reported as unknown.
    ready?: boolean;
    onapply: (p: Parsed) => void;
  } = $props();

  let pending: ReturnType<typeof setTimeout> | undefined;
  function apply(): void {
    clearTimeout(pending);
    pending = setTimeout(() => onapply(parseTokens(text, specs)), 250);
  }
  $effect(() => () => clearTimeout(pending));

  const unknown = $derived(ready ? parseTokens(text, specs).unknown : []);

  let focused = $state(false);
  let dismissed = $state(false);
  let active = $state(-1);
  const suggestions = $derived(suggest(text, specs));
  const listOpen = $derived(focused && !dismissed && suggestions.length > 0);

  function accept(s: Suggestion): void {
    text = complete(text, s);
    active = -1;
    apply();
    input?.focus();
  }

  function onkeydown(e: KeyboardEvent): void {
    if (!listOpen) return;
    const n = suggestions.length;
    if (e.key === 'ArrowDown') {
      active = (active + 1) % n;
    } else if (e.key === 'ArrowUp') {
      active = active <= 0 ? n - 1 : active - 1;
    } else if (e.key === 'Enter' && active >= 0) {
      accept(suggestions[active]!);
    } else if (e.key === 'Tab' && !e.shiftKey) {
      accept(suggestions[Math.max(active, 0)]!);
    } else if (e.key === 'Escape') {
      dismissed = true;
      e.stopPropagation();
    } else {
      return;
    }
    e.preventDefault();
  }
</script>

<div class="search-box search-combo">
  <label class="sr-only" for={id}>{label}</label>
  <input
    {id}
    type="search"
    role="combobox"
    autocomplete="off"
    aria-autocomplete="list"
    aria-expanded={listOpen}
    aria-controls="{id}-suggest"
    aria-activedescendant={listOpen && active >= 0 ? `${id}-suggest-${active}` : undefined}
    {placeholder}
    bind:value={text}
    bind:this={input}
    oninput={() => {
      dismissed = false;
      active = -1;
      apply();
    }}
    onfocus={() => (focused = true)}
    onblur={() => (focused = false)}
    {onkeydown}
  />
  {#if listOpen}
    <ul class="suggest" id="{id}-suggest" role="listbox" aria-label="Suggestions">
      {#each suggestions as s, i (s.label)}
        <!-- The box keeps focus: the list is picked with the arrow keys, Enter or Tab, or the pointer. -->
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <li
          id="{id}-suggest-{i}"
          role="option"
          aria-selected={i === active}
          class:active={i === active}
          onmousedown={(e) => e.preventDefault()}
          onclick={() => accept(s)}
        >
          <span class="mono">{s.label}</span>
          {#if s.hint}<span class="muted small">{s.hint}</span>{/if}
        </li>
      {/each}
    </ul>
  {/if}
</div>
{#if unknown.length}
  <p class="small muted token-note" role="note">Not filtering by {unknown.join(', ')}: nothing by that name.</p>
{/if}
