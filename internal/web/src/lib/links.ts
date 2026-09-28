// Route builders for DTOs that carry a repository full name rather than
// separate owner/repo fields.
import type { Route } from './routes';
import { splitRepo } from './format';

// connection names which connection holding the repository a route or
// action means; it is needed only where several hold the same owner/repo.
export function pullRoute(slug: string, p: { repository: string; number: number }, connection?: string): Route {
  const n = splitRepo(p.repository);
  return { name: 'pull', slug, owner: n.owner, repo: n.repo, number: p.number, ...(connection ? { connection } : {}) };
}

export function repoRoute(slug: string, fullName: string, connection?: string): Route {
  const n = splitRepo(fullName);
  return { name: 'repo', slug, owner: n.owner, repo: n.repo, ...(connection ? { connection } : {}) };
}

// connectionQuery is the ?connection= an API path carries, or ''.
export function connectionQuery(connection?: string): string {
  return connection ? `?connection=${encodeURIComponent(connection)}` : '';
}

// API paths for the dashboard actions.
function tenantApi(slug: string): string {
  return `/api/v1/tenants/${encodeURIComponent(slug)}`;
}

export function rerunPath(slug: string, p: { repository: string; number: number }, connection?: string): string {
  const n = splitRepo(p.repository);
  return `${tenantApi(slug)}/pulls/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/${p.number}/rerun${connectionQuery(connection)}`;
}

export function cancelPath(slug: string, reviewId: string): string {
  return `${tenantApi(slug)}/reviews/${encodeURIComponent(reviewId)}/cancel`;
}

export function reindexPath(slug: string, fullName: string, connection?: string): string {
  const n = splitRepo(fullName);
  return `${tenantApi(slug)}/repos/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/reindex${connectionQuery(connection)}`;
}
