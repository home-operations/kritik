package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
)

// PullRequestRow is a pull request as a webhook or a poll reports it.
type PullRequestRow struct {
	AccountID, RepositoryID          string
	Number                           int
	Title, Author                    string
	AuthorIsBot, Draft, Fork, Merged bool
	HeadRef, HeadSHA, BaseRef, URL   string
	Body                             string
	OpenedAt                         time.Time
	Labels                           json.RawMessage
}

// UpsertPullRequest records a pull request as open with what the forge
// says of it.
func UpsertPullRequest(ctx context.Context, tx pgx.Tx, p PullRequestRow) error {
	var opened any
	if !p.OpenedAt.IsZero() {
		opened = p.OpenedAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pull_requests (account_id, repository_id, number, title, author, author_is_bot, draft, fork, state,
			head_ref, head_sha, base_ref, url, body, opened_at, labels, merged)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (repository_id, number) DO UPDATE SET
			title = EXCLUDED.title, author = EXCLUDED.author, author_is_bot = EXCLUDED.author_is_bot, draft = EXCLUDED.draft,
			fork = EXCLUDED.fork, state = 'open', head_ref = EXCLUDED.head_ref, head_sha = EXCLUDED.head_sha,
			base_ref = EXCLUDED.base_ref, url = EXCLUDED.url, body = EXCLUDED.body,
			labels = EXCLUDED.labels, merged = EXCLUDED.merged, closed_at = NULL, updated_at = now()`,
		p.AccountID, p.RepositoryID, p.Number, p.Title, p.Author, p.AuthorIsBot, p.Draft, p.Fork,
		p.HeadRef, p.HeadSHA, p.BaseRef, p.URL, p.Body, opened, p.Labels, p.Merged); err != nil {
		return fmt.Errorf("store: upsert pull request: %w", err)
	}
	return nil
}

// ClosePullRequest records a pull request as closed, merged or not, when
// the forge says it closed; a nil closedAt is now.
func ClosePullRequest(ctx context.Context, tx pgx.Tx, repositoryID string, number int, merged bool, closedAt *time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'closed', merged = $3, closed_at = coalesce($4, now()), updated_at = now()
		WHERE repository_id = $1 AND number = $2`, repositoryID, number, merged, closedAt); err != nil {
		return fmt.Errorf("store: close pull request: %w", err)
	}
	return nil
}

// HeadReviewed reports whether any review has seen the head of the pull
// request.
func HeadReviewed(ctx context.Context, tx pgx.Tx, repositoryID string, number int, headSHA string) (bool, error) {
	var reviewed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviews r JOIN pull_requests p ON p.id = r.pull_request_id
		WHERE p.repository_id = $1 AND p.number = $2 AND r.head_sha = $3)`, repositoryID, number, headSHA).Scan(&reviewed); err != nil {
		return false, fmt.Errorf("store: read reviews of the head: %w", err)
	}
	return reviewed, nil
}

// RepositoryIndexed reports whether the repository has an active index
// generation.
func RepositoryIndexed(ctx context.Context, tx pgx.Tx, repositoryID string) (bool, error) {
	var indexed bool
	if err := tx.QueryRow(ctx, `SELECT active_index_run_id IS NOT NULL FROM repositories WHERE id = $1`, repositoryID).
		Scan(&indexed); err != nil {
		return false, fmt.Errorf("store: read index state: %w", err)
	}
	return indexed, nil
}

// DisableForgeRepositories disables the forge-reported repositories the
// App no longer reaches: the one named by repositoryID, or every one of
// the account when repositoryID is "". Repositories the configuration
// lists are left alone.
func DisableForgeRepositories(ctx context.Context, tx pgx.Tx, accountID, repositoryID string) error {
	if _, err := tx.Exec(ctx, `UPDATE repositories SET enabled = false, disabled_at = coalesce(disabled_at, now()), updated_at = now()
		WHERE account_id = $1 AND managed_by = 'forge' AND ($2 = '' OR id = $2::uuid)`, accountID, repositoryID); err != nil {
		return fmt.Errorf("store: disable repositories: %w", err)
	}
	return nil
}

// PollRepo is one enabled repository as a poll sees it. IndexedCommit is
// the commit its active index generation covers, "" when it has none.
type PollRepo struct {
	Name, DefaultBranch, IndexedCommit string
	Traits                             configfile.RepoTraits
}

// PollRepositories lists the account's enabled repositories, by name.
func PollRepositories(ctx context.Context, tx pgx.Tx) ([]PollRepo, error) {
	rows, err := tx.Query(ctx, `SELECT r.name, r.default_branch, coalesce(g.commit_sha, ''), r.archived, r.fork, r.turned_on
		FROM repositories r LEFT JOIN index_runs g ON g.id = r.active_index_run_id
		WHERE r.enabled ORDER BY r.name`)
	if err != nil {
		return nil, fmt.Errorf("store: list repositories to poll: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PollRepo, error) {
		var r PollRepo
		err := row.Scan(&r.Name, &r.DefaultBranch, &r.IndexedCommit, &r.Traits.Archived, &r.Traits.Fork, &r.Traits.TurnedOn)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list repositories to poll: %w", err)
	}
	return out, nil
}

// PollState is what a poll builds on: when the account became known, when
// it was last polled, and when its App's webhook last delivered, nil for
// never.
type PollState struct {
	Known             time.Time
	Polled, Delivered *time.Time
}

// ReadPollState reads the account's poll state against its App.
func ReadPollState(ctx context.Context, tx pgx.Tx, accountID, connectionID string) (PollState, error) {
	var s PollState
	if err := tx.QueryRow(ctx, `SELECT a.created_at, s.last_polled_at, c.last_webhook_at FROM accounts a
		CROSS JOIN connections c LEFT JOIN poll_state s ON s.account_id = a.id
		WHERE a.id = $1 AND c.id = $2`, accountID, connectionID).Scan(&s.Known, &s.Polled, &s.Delivered); err != nil {
		return s, fmt.Errorf("store: read poll state: %w", err)
	}
	return s, nil
}

// RecordPoll records when the account was last polled.
func RecordPoll(ctx context.Context, tx pgx.Tx, accountID string, at time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO poll_state (account_id, last_polled_at) VALUES ($1, $2)
		ON CONFLICT (account_id) DO UPDATE SET last_polled_at = excluded.last_polled_at, updated_at = now()`, accountID, at); err != nil {
		return fmt.Errorf("store: record poll: %w", err)
	}
	return nil
}

// ReviewedPull is a pull request with inline findings on the forge.
type ReviewedPull struct {
	ID, Repository string
	Number         int
}

// RecentlyReviewedPulls lists up to limit pull requests with inline
// findings from reviews since the given time, the most recently reviewed
// first.
func RecentlyReviewedPulls(ctx context.Context, tx pgx.Tx, since time.Time, limit int) ([]ReviewedPull, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, r.name, p.number FROM findings f
		JOIN reviews v ON v.id = f.review_id JOIN pull_requests p ON p.id = v.pull_request_id
		JOIN repositories r ON r.id = p.repository_id
		WHERE f.forge_comment_id IS NOT NULL AND v.created_at > $1
		GROUP BY p.id, r.name, p.number ORDER BY max(v.created_at) DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list reviewed pull requests: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReviewedPull, error) {
		var p ReviewedPull
		err := row.Scan(&p.ID, &p.Repository, &p.Number)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list reviewed pull requests: %w", err)
	}
	return out, nil
}

// Reaction is the reactions on one inline comment of the forge.
type Reaction struct {
	CommentID int64
	Up, Down  int
}

// RecordReactions writes the reactions of the pull request's inline
// findings, where they changed.
func RecordReactions(ctx context.Context, tx pgx.Tx, pullRequestID string, reactions []Reaction) error {
	ids := make([]int64, len(reactions))
	up := make([]int, len(reactions))
	down := make([]int, len(reactions))
	for i, r := range reactions {
		ids[i], up[i], down[i] = r.CommentID, r.Up, r.Down
	}
	if _, err := tx.Exec(ctx, `UPDATE findings f SET reactions_up = x.up, reactions_down = x.down
		FROM reviews v, unnest($2::bigint[], $3::int[], $4::int[]) AS x(id, up, down)
		WHERE v.id = f.review_id AND v.pull_request_id = $1 AND f.forge_comment_id = x.id
			AND (f.reactions_up, f.reactions_down) IS DISTINCT FROM (x.up, x.down)`, pullRequestID, ids, up, down); err != nil {
		return fmt.Errorf("store: record reactions: %w", err)
	}
	return nil
}
