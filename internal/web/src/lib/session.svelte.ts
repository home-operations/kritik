// Who is signed in and what the server allows, shared by every page that
// shows or hides a control on it. App.svelte loads both; pages only read.
import { getJSON } from './api.svelte';
import type { Me, Meta } from './types';

export const session = $state<{ me: Me | undefined; meta: Meta | undefined }>({ me: undefined, meta: undefined });

export const MANAGEMENT_OFF = 'Dashboard management is disabled: no sealing key configured.';

// loadMeta fetches /api/v1/meta once; it needs no session. A failure leaves
// meta unset, which reads as management off.
export async function loadMeta(): Promise<void> {
  if (session.meta) return;
  try {
    session.meta = await getJSON<Meta>('/api/v1/meta');
  } catch (err) {
    console.error('load meta:', err);
  }
}

// hookURL is a webhook path under the dashboard's URL, which the webhook
// listener shares.
export function hookURL(path: string): string {
  return (session.meta?.webUrl ?? '').replace(/\/+$/, '') + path;
}

export function management(): boolean {
  return session.meta?.management === true;
}

// canAdmin is whether the viewer may change account slug: /me gives an
// admin the admin role on every account, and anyone else member.
export function canAdmin(slug: string): boolean {
  const me = session.me;
  if (!me) return false;
  return me.operator || me.accounts.some((t) => t.slug === slug && t.role === 'admin');
}
