// j/k/Enter list navigation for a page, konflate-style. Ignored while typing,
// with a modifier held, or while an overlay (palette, help) is open.
import { help, palette } from './keyboard.svelte';

export interface ListKeys {
  count: () => number;
  get: () => number;
  set: (i: number) => void;
  open: (i: number) => void;
  focusSearch?: () => void;
}

function typing(e: KeyboardEvent): boolean {
  const el = e.target as HTMLElement | null;
  return !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable);
}

export function listKeys(k: ListKeys): () => void {
  const onKey = (e: KeyboardEvent) => {
    if (typing(e) || e.metaKey || e.ctrlKey || e.altKey || help.open || palette.open) return;
    const n = k.count();
    if (e.key === 'j' && n) k.set(Math.min(n - 1, k.get() + 1));
    else if (e.key === 'k' && n) k.set(Math.max(0, k.get() - 1));
    else if (e.key === 'Enter' && k.get() >= 0 && k.get() < n && !(e.target instanceof HTMLAnchorElement || e.target instanceof HTMLButtonElement)) k.open(k.get());
    else if (e.key === '/' && k.focusSearch) k.focusSearch();
    else return;
    e.preventDefault();
  };
  window.addEventListener('keydown', onKey);
  return () => window.removeEventListener('keydown', onKey);
}
