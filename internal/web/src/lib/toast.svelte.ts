// Transient result messages for actions, announced through one polite live
// region (components/Toasts.svelte) so a screen reader hears each result.
export type ToastTone = 'ok' | 'danger';

export interface Toast {
  id: number;
  tone: ToastTone;
  text: string;
}

export const toasts = $state<{ list: Toast[] }>({ list: [] });

let next = 0;

export function toast(text: string, tone: ToastTone = 'ok', ms = 6000): void {
  const id = ++next;
  toasts.list = [...toasts.list, { id, tone, text }];
  setTimeout(() => dismiss(id), ms);
}

export function dismiss(id: number): void {
  toasts.list = toasts.list.filter((t) => t.id !== id);
}
