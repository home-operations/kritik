// The dashboard's sections (ADR-0017): the tabs under the top bar, and the
// pages each holds. Rune-free, like routes.ts, so tests can import it.
import type { Route } from './routes';

export type Section = 'analytics' | 'pulls' | 'settings';

type Name = Route['name'];

interface SectionDef {
  label: string;
  // names are the routes the section's tab is current on.
  names: readonly Name[];
  // home is where the section's tab goes.
  home: (slug: string) => Route;
}

export const SECTIONS: Record<Section, SectionDef> = {
  analytics: { label: 'Analytics', names: ['account', 'findings', 'usage'], home: (slug) => ({ name: 'account', slug }) },
  pulls: { label: 'Pull requests', names: ['pulls', 'pull', 'review', 'queue', 'followups'], home: (slug) => ({ name: 'pulls', slug }) },
  settings: { label: 'Settings', names: ['repos', 'repo', 'admin', 'console'], home: (slug) => ({ name: 'repos', slug }) },
};

export const SECTION_ORDER: readonly Section[] = ['analytics', 'pulls', 'settings'];

export function sectionOf(r: Route): Section | undefined {
  return SECTION_ORDER.find((s) => SECTIONS[s].names.includes(r.name));
}

// A section's list pages share one strip of sub-tabs in place of a page
// title; a record's own page (a pull, a review) keeps its title instead.
export interface SubTab {
  label: string;
  name: Name;
  route: (slug: string) => Route;
}

export const SUB_TABS: Partial<Record<Section, readonly SubTab[]>> = {
  analytics: [
    { label: 'Reviews', name: 'account', route: (slug) => ({ name: 'account', slug }) },
    { label: 'Findings', name: 'findings', route: (slug) => ({ name: 'findings', slug }) },
    { label: 'Spend', name: 'usage', route: (slug) => ({ name: 'usage', slug }) },
  ],
  pulls: [
    { label: 'Pull requests', name: 'pulls', route: (slug) => ({ name: 'pulls', slug }) },
    { label: 'Queue', name: 'queue', route: (slug) => ({ name: 'queue', slug }) },
    { label: 'Follow-ups', name: 'followups', route: (slug) => ({ name: 'followups', slug }) },
  ],
};
