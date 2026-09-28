// What the setup wizard remembers in this browser: steps with nothing to
// save that the admin passed, and whether they closed it. Everything the
// wizard saves lives in the instance spec, so losing these only reopens a
// step.
import type { SetupStatus } from './types';

export interface SetupFlags {
  dismissed?: boolean;
  listener?: boolean;
  installed?: boolean;
  skipEmbedding?: boolean;
  repositories?: boolean;
}

const KEY = 'kritik.setup';

function read(): SetupFlags {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(KEY) ?? '{}');
    return typeof v === 'object' && v !== null ? (v as SetupFlags) : {};
  } catch {
    return {};
  }
}

export const setupFlags = $state<SetupFlags>(read());

export function setFlag<K extends keyof SetupFlags>(key: K, value: SetupFlags[K]): void {
  setupFlags[key] = value;
  try {
    localStorage.setItem(KEY, JSON.stringify(setupFlags));
  } catch {
    // Storage may be unavailable; the wizard then forgets across reloads.
  }
}

export const SETUP_STEPS = ['Listener', 'GitHub App', 'Model provider', 'Embeddings', 'Repositories', 'Done'] as const;

// firstStep is where the wizard resumes: the first step not yet done.
export function firstStep(s: SetupStatus, f: SetupFlags): number {
  if (!f.listener) return 0;
  if (s.connections.length === 0 || !f.installed) return 1;
  if (!s.reviewModel) return 2;
  if (!s.embedding && !f.skipEmbedding) return 3;
  if (!f.repositories) return 4;
  return 5;
}

// notReviewing says why the instance cannot review yet, '' when it can.
export function notReviewing(s: SetupStatus): string {
  if (s.connections.length === 0) return 'no GitHub App is connected';
  if (!s.reviewModel) return 'no review model is set';
  return '';
}
