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
