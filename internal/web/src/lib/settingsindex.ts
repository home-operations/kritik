// Every setting an admin can jump to, by the element each page marks it
// with: the admin console's sections, and an account's configuration, its
// sections and fields. The command palette and the settings navigation's
// search both read it. Rune-free so tests can import it.

export interface SettingEntry {
  label: string;
  // target selects the element to focus once its page shows it.
  target: string;
  keywords: string;
}

// The admin console's sections, in page order.
export const CONSOLE_SECTIONS: readonly SettingEntry[] = [
  { label: 'Instance configuration', target: '#op-config', keywords: 'settings spec json' },
  { label: 'Review defaults', target: '#instance-defaults', keywords: 'defaults models' },
  { label: 'Provider keys', target: '#instance-providers', keywords: 'model api key byok openrouter openai anthropic' },
  { label: 'Embeddings', target: '#instance-embedding', keywords: 'embedder index vector' },
  { label: 'Connections', target: '#instance-connections', keywords: 'github app webhook' },
  { label: 'Create a GitHub App', target: '#op-app', keywords: 'manifest register' },
  { label: 'GitHub installations', target: '#op-connections', keywords: 'uninstall connections' },
  { label: 'Instance settings', target: '#op-instance', keywords: 'environment' },
  { label: 'Admin audit log', target: '#op-audit', keywords: 'history' },
];

// The admin console's review defaults, by the spec path each carries.
export const CONSOLE_FIELDS: readonly SettingEntry[] = [
  { label: 'Default review model', target: '[data-path="defaults.models.review"]', keywords: 'models.review' },
  { label: 'Default fallback model', target: '[data-path="defaults.models.fallback"]', keywords: 'models.fallback' },
  { label: 'Default mode', target: '[data-path="defaults.mode"]', keywords: 'single agentic' },
  { label: 'Default thoroughness', target: '[data-path="defaults.review.thoroughness"]', keywords: 'review focused thorough nits line comments' },
  { label: 'Default forks', target: '[data-path="defaults.forks"]', keywords: '' },
  { label: 'Default settle', target: '[data-path="defaults.settle"]', keywords: 'delay' },
];

// An account's configuration, by section, in page order.
export const ACCOUNT_SECTIONS: readonly SettingEntry[] = [
  { label: 'Reviews', target: '#account-reviews', keywords: 'model mode filter thoroughness forks settle' },
  { label: 'Limits', target: '#account-limits', keywords: 'concurrency budget caps runner' },
  { label: 'Provider keys', target: '#account-providers', keywords: 'model api key byok' },
  { label: 'Repositories', target: '#account-repositories', keywords: 'mode enabled' },
];

// An account's configuration fields, by the spec path each carries.
export const ACCOUNT_FIELDS: readonly SettingEntry[] = [
  { label: 'Review model', target: '[data-path="models.review"]', keywords: 'models.review' },
  { label: 'Fallback model', target: '[data-path="models.fallback"]', keywords: 'models.fallback' },
  { label: 'Mode', target: '[data-path="mode"]', keywords: 'single agentic' },
  { label: 'Filter', target: '[data-path="filter"]', keywords: 'cel' },
  { label: 'Forks', target: '[data-path="forks"]', keywords: '' },
  { label: 'Thoroughness', target: '[data-path="review.thoroughness"]', keywords: 'review focused thorough nits line comments' },
  { label: 'Settle', target: '[data-path="settle"]', keywords: 'delay' },
  { label: 'Concurrency', target: '[data-path="limits.concurrency"]', keywords: 'limits' },
  { label: 'Reviews per day', target: '[data-path="limits.reviewsPerDay"]', keywords: 'limits' },
  { label: 'Tokens per month', target: '[data-path="limits.tokensPerMonth"]', keywords: 'limits budget spend' },
  { label: 'Runner', target: '[data-path="runner"]', keywords: 'deadline resources' },
  { label: 'Provider keys', target: '#account-providers', keywords: 'model api key byok' },
  { label: 'Repositories', target: '#account-repositories', keywords: 'mode enabled' },
];

// matches reports whether an entry's label or keywords contain needle.
export function matches(e: SettingEntry, needle: string): boolean {
  const n = needle.trim().toLowerCase();
  return !n || e.label.toLowerCase().includes(n) || e.keywords.toLowerCase().includes(n);
}
