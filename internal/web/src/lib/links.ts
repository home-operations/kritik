// Route builders for DTOs that carry a repository full name rather than
// separate owner/repo fields, and the API paths of an account.
import { slugPath, type Route } from './routes';
import { splitRepo } from './format';
import { safeHref } from './markdown';

// pullKey names a pull request across the account's repositories.
export function pullKey(p: { repository: string; number: number }): string {
  return `${p.repository}#${p.number}`;
}

export function pullRoute(slug: string, p: { repository: string; number: number }): Route {
  const n = splitRepo(p.repository);
  return { name: 'pull', slug, owner: n.owner, repo: n.repo, number: p.number };
}

export function repoRoute(slug: string, fullName: string): Route {
  const n = splitRepo(fullName);
  return { name: 'repo', slug, owner: n.owner, repo: n.repo };
}

// accountApi is the API path of the account slug.
export function accountApi(slug: string): string {
  return `/api/v1/accounts/${slugPath(slug)}`;
}

export function rerunPath(slug: string, p: { repository: string; number: number }): string {
  const n = splitRepo(p.repository);
  return `${accountApi(slug)}/pulls/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/${p.number}/rerun`;
}

// threadUrl links the thread of an inline comment on a pull request; none
// for a finding kritika did not post inline, or whose comment id it lacks.
export function threadUrl(pullUrl: string, commentId: number | null): string | undefined {
  return commentId ? safeHref(`${pullUrl}#discussion_r${commentId}`) : undefined;
}

export function cancelPath(slug: string, reviewId: string): string {
  return `${accountApi(slug)}/reviews/${encodeURIComponent(reviewId)}/cancel`;
}

export function reindexPath(slug: string, fullName: string): string {
  const n = splitRepo(fullName);
  return `${accountApi(slug)}/repos/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/reindex`;
}

export function turnedOnPath(slug: string, fullName: string): string {
  const n = splitRepo(fullName);
  return `${accountApi(slug)}/repos/${encodeURIComponent(n.owner)}/${encodeURIComponent(n.repo)}/turned-on`;
}
