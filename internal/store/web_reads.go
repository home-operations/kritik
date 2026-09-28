package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The dashboard's read queries. Each takes a transaction opened by
// WithTenant, so row-level security confines it to that tenant; none of
// them filters by tenant_id itself.

// ReviewStatus is a reviews.status value.
type ReviewStatus string

// Review statuses, as the reviews_status_check constraint lists them.
const (
	ReviewRunning    ReviewStatus = "running"
	ReviewPrepared   ReviewStatus = "prepared"
	ReviewCompleted  ReviewStatus = "completed"
	ReviewSuperseded ReviewStatus = "superseded"
	ReviewSkipped    ReviewStatus = "skipped"
	ReviewCapped     ReviewStatus = "capped"
	ReviewFailed     ReviewStatus = "failed"
	ReviewCanceled   ReviewStatus = "canceled"
)

// Valid reports whether s is a review status.
func (s ReviewStatus) Valid() bool {
	switch s {
	case ReviewRunning, ReviewPrepared, ReviewCompleted, ReviewSuperseded, ReviewSkipped, ReviewCapped, ReviewFailed, ReviewCanceled:
		return true
	}
	return false
}

// IndexRunStatus is an index_runs.status value.
type IndexRunStatus string

// Index run statuses.
const (
	IndexRunning    IndexRunStatus = "running"
	IndexCompleted  IndexRunStatus = "completed"
	IndexFailed     IndexRunStatus = "failed"
	IndexSuperseded IndexRunStatus = "superseded"
)

// Valid reports whether s is an index run status.
func (s IndexRunStatus) Valid() bool {
	return s == IndexRunning || s == IndexCompleted || s == IndexFailed || s == IndexSuperseded
}

// FollowupStatus is a followups.status value.
type FollowupStatus string

// Follow-up statuses.
const (
	FollowupAnswered FollowupStatus = "answered"
	FollowupLimited  FollowupStatus = "limited"
	FollowupIgnored  FollowupStatus = "ignored"
	FollowupFailed   FollowupStatus = "failed"
)

// Valid reports whether s is a follow-up status.
func (s FollowupStatus) Valid() bool {
	return s == FollowupAnswered || s == FollowupLimited || s == FollowupIgnored || s == FollowupFailed
}

// PullState filters pull requests by pull_requests.state.
type PullState string

// Pull request states a list may ask for; PullAll matches both.
const (
	PullOpen   PullState = "open"
	PullClosed PullState = "closed"
	PullAll    PullState = "all"
)

// Valid reports whether s is a pull state filter.
func (s PullState) Valid() bool { return s == PullOpen || s == PullClosed || s == PullAll }

// Cursor is the position after the last row of a page: the sort key of that
// row (T for a time-ordered list, S for a text-ordered one) and its id, the
// tiebreak. The zero Cursor is the first page.
type Cursor struct {
	T  time.Time `json:"t,omitzero"`
	S  string    `json:"s,omitempty"`
	ID string    `json:"id,omitempty"`
}

// First reports whether c is the start of a list.
func (c Cursor) First() bool { return c.ID == "" }

// Page bounds one keyset-paginated read.
type Page struct {
	After Cursor
	Limit int
}

// ErrPageLimit is a page whose limit is not positive.
var ErrPageLimit = errors.New("store: page limit must be positive")

// check rejects a page with no room or a cursor whose id is not a row id.
func (p Page) check() error {
	if p.Limit <= 0 {
		return ErrPageLimit
	}
	if !p.After.First() && uuid.Validate(p.After.ID) != nil {
		return ErrFilter
	}
	return nil
}

// afterID is the cursor's id as a uuid parameter, NULL on the first page.
func (p Page) afterID() any { return uuidParam(p.After.ID) }

// uuidParam passes s as a uuid parameter, NULL when it is empty, so a
// query can say "($1::uuid IS NULL OR col = $1)" and still use col's index.
func uuidParam(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// paged trims a query's limit+1 rows to the page, returning the cursor of
// the next page or nil at the end of the list.
func paged[T any](rows []T, limit int, key func(T) Cursor) ([]T, *Cursor) {
	if len(rows) <= limit {
		return rows, nil
	}
	rows = rows[:limit]
	next := key(rows[limit-1])
	return rows, &next
}

// TenantStats is what the tenant list shows of one tenant.
type TenantStats struct {
	Connections  int
	Repositories int
	Reviews7d    int
	Month        MonthUsage
}

// MonthUsage is what a tenant's caps count: tokens and spend this calendar
// month and completed reviews today.
type MonthUsage struct {
	Tokens       int64
	CostUSD      float64
	ReviewsToday int64
}

// ReadTenantStats reads the tenant's counts. The month and day boundaries
// are the database's, as the worker's cap checks use.
func ReadTenantStats(ctx context.Context, tx pgx.Tx) (TenantStats, error) {
	var s TenantStats
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM connections WHERE enabled),
		(SELECT count(*) FROM repositories WHERE enabled),
		(SELECT count(*) FROM reviews WHERE created_at >= now() - interval '7 days')`).
		Scan(&s.Connections, &s.Repositories, &s.Reviews7d)
	if err != nil {
		return s, fmt.Errorf("store: tenant stats: %w", err)
	}
	if s.Month, err = ReadMonthUsage(ctx, tx); err != nil {
		return s, err
	}
	return s, nil
}

// ReadMonthUsage reads the tenant's month-to-date usage.
func ReadMonthUsage(ctx context.Context, tx pgx.Tx) (MonthUsage, error) {
	var m MonthUsage
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(input_tokens + output_tokens), 0), coalesce(sum(cost_usd), 0)::float8,
		(SELECT count(*) FROM reviews WHERE status = 'completed' AND created_at >= date_trunc('day', now()))
		FROM usage WHERE created_at >= date_trunc('month', now())`).
		Scan(&m.Tokens, &m.CostUSD, &m.ReviewsToday)
	if err != nil {
		return m, fmt.Errorf("store: month usage: %w", err)
	}
	return m, nil
}

// ReadWebhookDeliveries reads when each of the tenant's connections last
// received a verified webhook, keyed by connection id; one that never has
// is absent.
func ReadWebhookDeliveries(ctx context.Context, tx pgx.Tx) (map[string]time.Time, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, last_webhook_at FROM connections WHERE last_webhook_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: webhook deliveries: %w", err)
	}
	out := map[string]time.Time{}
	var id string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &at}, func() error {
		out[id] = at
		return nil
	}); err != nil {
		return nil, fmt.Errorf("store: webhook deliveries: %w", err)
	}
	return out, nil
}

// RepoRow is one repository as the repository list shows it.
type RepoRow struct {
	ID            string
	FullName      string
	ConnectionID  string
	Connection    string
	Enabled       bool
	ManagedBy     string
	DefaultBranch string
	// ActiveCommit and ActiveAt describe the active index generation, empty
	// when there is none.
	ActiveCommit string
	ActiveAt     *time.Time
	// LastIndexStatus and LastIndexAt describe the newest index run.
	LastIndexStatus IndexRunStatus
	LastIndexAt     *time.Time
	LastReview      *ReviewRef
}

// ReviewRef is a review's id, status and when it began.
type ReviewRef struct {
	ID        string
	Status    ReviewStatus
	CreatedAt time.Time
}

const repoColumns = `r.id, r.name, r.connection_id, i.name, r.enabled, r.managed_by, r.default_branch,
	coalesce(a.commit_sha, ''), coalesce(a.finished_at, a.created_at),
	coalesce(l.status, ''), l.created_at,
	lr.id, lr.status, lr.created_at
	FROM repositories r
	JOIN connections i ON i.id = r.connection_id
	LEFT JOIN index_runs a ON a.id = r.active_index_run_id
	LEFT JOIN LATERAL (SELECT status, created_at FROM index_runs x WHERE x.repository_id = r.id
		ORDER BY created_at DESC, id DESC LIMIT 1) l ON true
	LEFT JOIN LATERAL (SELECT v.id, v.status, v.created_at FROM reviews v JOIN pull_requests p ON p.id = v.pull_request_id
		WHERE p.repository_id = r.id ORDER BY v.created_at DESC, v.id DESC LIMIT 1) lr ON true`

func scanRepo(row pgx.CollectableRow) (RepoRow, error) {
	var r RepoRow
	var status string
	var lrID, lrStatus *string
	var lrAt *time.Time
	err := row.Scan(&r.ID, &r.FullName, &r.ConnectionID, &r.Connection, &r.Enabled, &r.ManagedBy, &r.DefaultBranch,
		&r.ActiveCommit, &r.ActiveAt, &status, &r.LastIndexAt, &lrID, &lrStatus, &lrAt)
	r.LastIndexStatus = IndexRunStatus(status)
	if r.ActiveCommit == "" {
		r.ActiveAt = nil
	}
	if lrID != nil && lrStatus != nil && lrAt != nil {
		r.LastReview = &ReviewRef{ID: *lrID, Status: ReviewStatus(*lrStatus), CreatedAt: *lrAt}
	}
	return r, err
}

// ListRepos returns a page of the tenant's repositories ordered by full
// name.
func ListRepos(ctx context.Context, tx pgx.Tx, p Page) ([]RepoRow, *Cursor, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+repoColumns+`
		WHERE $1 OR (r.name, r.id) > ($2, $3::uuid)
		ORDER BY r.name, r.id LIMIT $4`, p.After.First(), p.After.S, p.afterID(), p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list repositories: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanRepo)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list repositories: %w", err)
	}
	items, next := paged(out, p.Limit, func(r RepoRow) Cursor { return Cursor{S: r.FullName, ID: r.ID} })
	return items, next, nil
}

// AmbiguousRepoError is FindRepo's answer when several connections of
// the tenant hold a repository of the name asked for and the caller named
// none of them.
type AmbiguousRepoError struct {
	Connections []string
}

func (e *AmbiguousRepoError) Error() string {
	return "store: several connections hold this repository: " + strings.Join(e.Connections, ", ")
}

// FindRepo returns the tenant's repository named fullName, reached through
// connection when it is set. Without it, a name several connections
// hold is an *AmbiguousRepoError, except that an enabled repository wins
// over disabled ones, which a removed or renamed connection leaves
// behind.
func FindRepo(ctx context.Context, tx pgx.Tx, fullName, connection string) (RepoRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+repoColumns+`
		WHERE r.name = $1 AND ($2 = '' OR i.name = $2)
		ORDER BY r.enabled DESC, r.created_at, r.id`, fullName, connection)
	if err != nil {
		return RepoRow{}, fmt.Errorf("store: find repository: %w", err)
	}
	repos, err := pgx.CollectRows(rows, scanRepo)
	if err != nil {
		return RepoRow{}, fmt.Errorf("store: find repository: %w", err)
	}
	if len(repos) == 0 {
		return RepoRow{}, ErrNotFound
	}
	if len(repos) > 1 && repos[1].Enabled == repos[0].Enabled {
		e := &AmbiguousRepoError{}
		for _, r := range repos {
			if r.Enabled == repos[0].Enabled {
				e.Connections = append(e.Connections, r.Connection)
			}
		}
		return RepoRow{}, e
	}
	return repos[0], nil
}

// IndexRunRow is one index_runs row.
type IndexRunRow struct {
	ID           string
	RepositoryID string
	Repository   string
	CommitSHA    string
	BaseSHA      string
	EmbedModel   string
	Mode         string
	Status       IndexRunStatus
	Trigger      string
	ChunkCount   int
	Error        string
	CreatedAt    time.Time
	FinishedAt   *time.Time
}

// ListIndexRuns returns a page of index runs, newest first, of one
// repository when repositoryID is set.
func ListIndexRuns(ctx context.Context, tx pgx.Tx, repositoryID string, p Page) ([]IndexRunRow, *Cursor, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT x.id, x.repository_id, r.name, x.commit_sha, x.base_sha, x.embed_model, x.mode, x.status,
		x.trigger, x.chunk_count, x.error, x.created_at, x.finished_at
		FROM index_runs x JOIN repositories r ON r.id = x.repository_id
		WHERE ($1::uuid IS NULL OR x.repository_id = $1) AND ($2 OR (x.created_at, x.id) < ($3, $4::uuid))
		ORDER BY x.created_at DESC, x.id DESC LIMIT $5`,
		uuidParam(repositoryID), p.After.First(), p.After.T, p.afterID(), p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list index runs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IndexRunRow, error) {
		var x IndexRunRow
		var status string
		err := row.Scan(&x.ID, &x.RepositoryID, &x.Repository, &x.CommitSHA, &x.BaseSHA, &x.EmbedModel, &x.Mode, &status,
			&x.Trigger, &x.ChunkCount, &x.Error, &x.CreatedAt, &x.FinishedAt)
		x.Status = IndexRunStatus(status)
		return x, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: list index runs: %w", err)
	}
	items, next := paged(out, p.Limit, func(x IndexRunRow) Cursor { return Cursor{T: x.CreatedAt, ID: x.ID} })
	return items, next, nil
}

// RepoFileRow is the repository's .kritik.yaml as the last review that ran
// read it: the review, the merge base it read the file at, and the file,
// nil when there was none there.
type RepoFileRow struct {
	ReviewID string
	Commit   string
	Doc      *string
}

// LastRepoFile reads the .kritik.yaml the repository's last review with a
// context pack read; ErrNotFound when no review has one yet.
func LastRepoFile(ctx context.Context, tx pgx.Tx, repositoryID string) (RepoFileRow, error) {
	var row RepoFileRow
	err := tx.QueryRow(ctx, `
		SELECT r.id, c.base_sha, c.repo_files ->> '.kritik.yaml'
		FROM reviews r JOIN pull_requests p ON p.id = r.pull_request_id
		JOIN runner_runs rr ON rr.review_id = r.id JOIN context_packs c ON c.runner_run_id = rr.id
		WHERE p.repository_id = $1 ORDER BY c.created_at DESC LIMIT 1`, repositoryID).Scan(&row.ReviewID, &row.Commit, &row.Doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoFileRow{}, ErrNotFound
	}
	if err != nil {
		return RepoFileRow{}, fmt.Errorf("store: last repository file: %w", err)
	}
	return row, nil
}
