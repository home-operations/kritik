<script lang="ts" generics="D">
  // The shell a spec form sits in: a draft of the spec, a JSON view for
  // what the fields have no control for, and the save. fields renders the
  // form over the draft; the caller does the request and passes back any
  // error, whose path highlights and focuses the field it names.
  import { tick, untrack, type Snippet } from 'svelte';
  import { pathMatches, type Built, type SpecError } from '../../spec';

  type Obj = Record<string, unknown>;

  interface Props {
    initial: Obj;
    // draftOf and build convert between the spec and its draft; build
    // shows each secret as keep when redact is set, for the JSON view.
    draftOf: (spec: Obj) => D;
    build: (draft: D, redact?: boolean) => Built;
    // hasTypedSecret says whether the draft holds a typed secret, which
    // the JSON view never shows.
    hasTypedSecret: (draft: D) => boolean;
    // fields gets the draft, whether a path is the one in error, and
    // structural, which wraps an edit that shifts indexes.
    fields: Snippet<[D, (path: string) => boolean, (edit: () => void) => void]>;
    // jsonNotice says what the JSON view holds and how secrets read in it.
    jsonNotice: Snippet;
    saving: boolean;
    // The last failed save's message and the spec path it points at.
    errMessage?: string;
    errPath?: string;
    // Bumped on every failed save, so a repeated error still refocuses.
    errSeq?: number;
    alertAction?: Snippet;
    dirty?: boolean;
    onsave: (spec: Obj) => void | Promise<void>;
  }
  let {
    initial,
    draftOf,
    build,
    hasTypedSecret,
    fields,
    jsonNotice,
    saving,
    errMessage = '',
    errPath = '',
    errSeq = 0,
    alertAction,
    dirty = $bindable(false),
    onsave,
  }: Props = $props();

  let draft = $state<D>(untrack(() => draftOf(initial)));
  const baseline = untrack(() => JSON.stringify(build(draftOf(initial)).spec));
  let clientError = $state<SpecError | undefined>(undefined);
  let jsonMode = $state(false);
  let jsonText = $state('');
  let jsonEntered = '';
  let formEl = $state<HTMLFormElement | undefined>(undefined);

  $effect(() => {
    dirty = JSON.stringify(build(draft).spec) !== baseline || (jsonMode && jsonText !== jsonEntered);
  });

  // A structural edit shifts indexes, so a server error pointing at a path
  // is dismissed by one; clearedSeq is the errSeq dismissed.
  let clearedSeq = $state(-1);
  const serverShown = $derived(clearedSeq !== errSeq);
  const activePath = $derived(clientError ? clientError.path : serverShown ? errPath : '');
  const alert = $derived(
    clientError ? `${clientError.path ? `${clientError.path}: ` : ''}${clientError.message}` : serverShown ? errMessage : '',
  );
  const inv = (path: string) => pathMatches(path, activePath);

  function structural(edit: () => void): void {
    edit();
    clientError = undefined;
    if (errPath) clearedSeq = errSeq;
  }

  function focusPath(path: string): void {
    if (!path || !formEl) return;
    for (const el of formEl.querySelectorAll<HTMLElement>('[data-path]')) {
      if (!pathMatches(el.dataset.path ?? '', path)) continue;
      const target = el.matches('input, select, textarea') ? el : el.querySelector<HTMLElement>('input, select, textarea');
      target?.focus();
      target?.scrollIntoView({ block: 'center' });
      return;
    }
  }

  // A new server error moves focus to the field it names.
  $effect(() => {
    void errSeq;
    const p = untrack(() => errPath);
    if (p && !untrack(() => jsonMode)) void tick().then(() => focusPath(p));
  });

  function parseJSON(): Obj | undefined {
    try {
      const v: unknown = JSON.parse(jsonText);
      if (typeof v === 'object' && v !== null && !Array.isArray(v)) return v as Obj;
      clientError = { path: '', message: 'the spec must be a JSON object' };
    } catch (err) {
      clientError = { path: '', message: `the JSON does not parse: ${err instanceof Error ? err.message : String(err)}` };
    }
    return undefined;
  }

  function toggleJSON(): void {
    clientError = undefined;
    if (!jsonMode) {
      // The JSON view never shows a typed secret, so switching would lose it.
      if (hasTypedSecret(draft)) {
        clientError = {
          path: '',
          message: 'Save, or clear, the secret values typed into the form first: the JSON view never shows them, so they would be lost.',
        };
        return;
      }
      jsonText = JSON.stringify(build(draft, true).spec, null, 2);
      jsonEntered = jsonText;
      jsonMode = true;
      return;
    }
    const v = parseJSON();
    if (!v) return;
    draft = draftOf(v);
    jsonMode = false;
    jsonText = '';
  }

  async function submit(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    clientError = undefined;
    if (jsonMode) {
      const v = parseJSON();
      if (v) await onsave(v);
      return;
    }
    const b = build(draft);
    if (b.error) {
      clientError = b.error;
      await tick();
      focusPath(b.error.path);
      return;
    }
    await onsave(b.spec);
  }
</script>

<form class="form" bind:this={formEl} onsubmit={submit} novalidate>
  <div class="form-actions">
    <button type="button" class="btn" aria-pressed={jsonMode} onclick={toggleJSON}>
      {jsonMode ? 'Back to the form' : 'Advanced: edit JSON'}
    </button>
  </div>

  {#if jsonMode}
    <p class="notice">{@render jsonNotice()}</p>
    <label class="field">
      <span>Spec JSON</span>
      <textarea class="json-edit" spellcheck="false" bind:value={jsonText} aria-invalid={!!clientError || undefined}></textarea>
    </label>
  {:else}
    {@render fields(draft, inv, structural)}
  {/if}

  <div aria-live="assertive">
    {#if alert}
      <div class="form-alert" role="alert">
        <span>{alert}</span>
        {#if alertAction && !clientError}{@render alertAction()}{/if}
      </div>
    {/if}
  </div>

  <div class="form-actions">
    <button type="submit" class="btn btn-primary" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
    {#if dirty}<span class="field-hint">Unsaved changes</span>{/if}
  </div>
</form>
