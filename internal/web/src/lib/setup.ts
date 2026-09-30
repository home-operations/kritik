// What an instance still lacks before it reviews (ADR-0019 §2.2), from its
// setup status and the accounts its connections serve. Everything it names
// is set in the configuration file. Rune-free so tests can import it.
import type { AdminAccount, SetupStatus } from './types';

export interface ChecklistItem {
  label: string;
  done: boolean;
  // optional is a step kritik reviews without.
  optional?: boolean;
  // how says what to do while the step is not done.
  how: string;
}

export function checklist(s: SetupStatus, accounts: AdminAccount[]): ChecklistItem[] {
  return [
    {
      label: 'A GitHub App is connected',
      done: s.connections.length > 0,
      how: 'Declare the App under apps in the configuration file, its private key and webhook secret referenced from a Secret.',
    },
    {
      label: 'The App reaches a repository',
      done: accounts.some((a) => a.live && a.repositories > 0),
      how: 'Install the App on GitHub on an account its entry under apps lists.',
    },
    {
      label: 'A review model is set',
      done: s.reviewModel !== '',
      how: 'Set defaults.models.review to a model one of the providers serves.',
    },
    {
      label: 'An embedder is set',
      done: s.embedding,
      optional: true,
      how: 'Set embedding to index each repository for similar code; reviews run without it.',
    },
  ];
}

// notReviewing says why the instance cannot review yet, '' when it can.
export function notReviewing(s: SetupStatus): string {
  if (s.connections.length === 0) return 'no GitHub App is connected';
  if (!s.reviewModel) return 'no review model is set';
  return '';
}
