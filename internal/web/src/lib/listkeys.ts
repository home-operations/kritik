// j/k/Enter list navigation for a page, konflate-style. Ignored while typing,
// with a modifier held, or while an overlay (palette, help) is open.
import { help, isTyping, palette } from './keyboard.svelte';

export interface ListKeys {
  count: () => number;
  get: () => number;
  set: (i: number) => void;
  open: (i: number) => void;
  focusSearch?: () => void;
  // toggle picks or unpicks row i for a bulk action, on Space.
  toggle?: (i: number) => void;
}

export function listKeys(k: ListKeys): () => void {
  const onKey = (e: KeyboardEvent) => {
    if (isTyping(e) || e.metaKey || e.ctrlKey || e.altKey || help.open || palette.open) return;
    const n = k.count();
    if (e.key === 'j' && n) k.set(Math.min(n - 1, k.get() + 1));
    else if (e.key === 'k' && n) k.set(Math.max(0, k.get() - 1));
    else if (e.key === 'Enter' && k.get() >= 0 && k.get() < n && !(e.target instanceof HTMLAnchorElement || e.target instanceof HTMLButtonElement)) k.open(k.get());
    else if (e.key === '/' && k.focusSearch) k.focusSearch();
    else if (e.key === ' ' && k.toggle && k.get() >= 0 && k.get() < n && !(e.target instanceof HTMLButtonElement)) k.toggle(k.get());
    else return;
    e.preventDefault();
  };
  window.addEventListener('keydown', onKey);
  return () => window.removeEventListener('keydown', onKey);
}
