// Route builders for DTOs that carry a repository full name rather than
// separate owner/repo fields.
import type { Route } from './routes';
import { splitRepo } from './format';

export function pullRoute(slug: string, p: { repository: string; number: number }): Route {
  const n = splitRepo(p.repository);
  return { name: 'pull', slug, owner: n.owner, repo: n.repo, number: p.number };
}

export function repoRoute(slug: string, fullName: string): Route {
  const n = splitRepo(fullName);
  return { name: 'repo', slug, owner: n.owner, repo: n.repo };
}

// API paths for the dashboard actions.
function tenantApi(slug: string): string {
  return `/api/v1/tenants/${encodeURIComponent(slug)}`;
}

export function rerunPath(slug: string, p: { repository: string; number: number }): string {
  const n = splitRepo(p.repository);
  return `${tenantApi(slug)}/pulls/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/${p.number}/rerun`;
}

export function cancelPath(slug: string, reviewId: string): string {
  return `${tenantApi(slug)}/reviews/${encodeURIComponent(reviewId)}/cancel`;
}

export function reindexPath(slug: string, fullName: string): string {
  const n = splitRepo(fullName);
  return `${tenantApi(slug)}/repos/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/reindex`;
}
