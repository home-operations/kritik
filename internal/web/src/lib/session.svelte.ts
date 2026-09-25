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

export function management(): boolean {
  return session.meta?.management === true;
}

export function isOperator(): boolean {
  return session.me?.operator === true;
}

// canAdmin mirrors the server's CanAdmin: an operator administers every
// tenant, anyone else only the tenants they are an admin of.
export function canAdmin(slug: string): boolean {
  const me = session.me;
  if (!me) return false;
  return me.operator || me.tenants.some((t) => t.slug === slug && t.role === 'admin');
}
