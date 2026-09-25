package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/home-operations/kritik/internal/configfile"
	"github.com/home-operations/kritik/internal/repoconfig"
	"github.com/home-operations/kritik/internal/review"
)

// Label is one pull request label as pull_requests.labels records it.
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// SeverityCounts counts a review's findings by severity.
type SeverityCounts struct {
	Blocking, Important, Nit int
}

// ReviewBrief is the newest review of a pull request as the pull request
// list shows it.
type ReviewBrief struct {
	ID        string
	Status    ReviewStatus
	Mode      configfile.ReviewMode
	Scope     review.Scope
	Findings  SeverityCounts
	CreatedAt time.Time
}

// PullRow is one pull request with its newest review.
type PullRow struct {
	ID           string
	RepositoryID string
	Repository   string
	Number       int
	Title        string
	Author       string
	State        string
	Draft        bool
	Merged       bool
	HeadSHA      string
	HeadRef      string
	BaseRef      string
	URL          string
	OpenedAt     *time.Time
	UpdatedAt    time.Time
	Labels       []Label
	LastReview   *ReviewBrief
}

// PullFilter narrows ListPulls. Zero fields match everything; Query
// matches a title or author substring, or a number.
type PullFilter struct {
	RepositoryID string
	State        PullState
	Outcome      ReviewStatus
	Query        string
}

const pullColumns = `p.id, p.repository_id, r.name, p.number, p.title, p.author, p.state, p.draft, p.merged,
	p.head_sha, p.head_ref, p.base_ref, p.url, p.opened_at, p.updated_at, p.labels,
	lr.id, lr.status, lr.mode, lr.scope, lr.created_at, lr.blocking, lr.important, lr.nit
	FROM pull_requests p JOIN repositories r ON r.id = p.repository_id
	LEFT JOIN LATERAL (SELECT v.id, v.status, v.mode, v.scope, v.created_at,
		count(f.id) FILTER (WHERE f.severity = 'blocking') AS blocking,
		count(f.id) FILTER (WHERE f.severity = 'important') AS important,
		count(f.id) FILTER (WHERE f.severity = 'nit') AS nit
		FROM reviews v LEFT JOIN findings f ON f.review_id = v.id
		WHERE v.id = (SELECT id FROM reviews WHERE pull_request_id = p.id ORDER BY created_at DESC, id DESC LIMIT 1)
		GROUP BY v.id) lr ON true`

func scanPull(row pgx.CollectableRow) (PullRow, error) {
	var p PullRow
	var labels []byte
	var id, status, mode, scope *string
	var at *time.Time
	var blocking, important, nit *int
	if err := row.Scan(&p.ID, &p.RepositoryID, &p.Repository, &p.Number, &p.Title, &p.Author, &p.State, &p.Draft, &p.Merged,
		&p.HeadSHA, &p.HeadRef, &p.BaseRef, &p.URL, &p.OpenedAt, &p.UpdatedAt, &labels,
		&id, &status, &mode, &scope, &at, &blocking, &important, &nit); err != nil {
		return p, err
	}
	p.Labels = []Label{}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &p.Labels); err != nil {
			return p, fmt.Errorf("decode labels: %w", err)
		}
	}
	if id != nil {
		p.LastReview = &ReviewBrief{
			ID: *id, Status: ReviewStatus(*status), Mode: configfile.ReviewMode(*mode), Scope: review.Scope(*scope), CreatedAt: *at,
			Findings: SeverityCounts{Blocking: *blocking, Important: *important, Nit: *nit},
		}
	}
	return p, nil
}

// ErrFilter is a list filter with a value outside its type.
var ErrFilter = errors.New("store: invalid filter")

// ListPulls returns a page of pull requests, most recently updated first.
func ListPulls(ctx context.Context, tx pgx.Tx, f PullFilter, p Page) ([]PullRow, *Cursor, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	if f.State == "" {
		f.State = PullAll
	}
	if !f.State.Valid() || (f.Outcome != "" && !f.Outcome.Valid()) {
		return nil, nil, ErrFilter
	}
	number := -1
	if n, err := strconv.Atoi(strings.TrimPrefix(f.Query, "#")); err == nil {
		number = n
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query) + "%"
	rows, err := tx.Query(ctx, `SELECT `+pullColumns+`
		WHERE ($1::uuid IS NULL OR p.repository_id = $1)
			AND ($2 = 'all' OR p.state = $2)
			AND ($3 = '' OR lr.status = $3)
			AND ($4 = '' OR p.title ILIKE $5 OR p.author ILIKE $5 OR p.number = $6)
			AND ($7 OR (p.updated_at, p.id) < ($8, $9::uuid))
		ORDER BY p.updated_at DESC, p.id DESC LIMIT $10`,
		uuidParam(f.RepositoryID), string(f.State), string(f.Outcome), f.Query, like, number,
		p.After.First(), p.After.T, p.afterID(), p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list pull requests: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanPull)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list pull requests: %w", err)
	}
	items, next := paged(out, p.Limit, func(r PullRow) Cursor { return Cursor{T: r.UpdatedAt, ID: r.ID} })
	return items, next, nil
}

// FindPull returns the pull request numbered number of a repository.
func FindPull(ctx context.Context, tx pgx.Tx, repositoryID string, number int) (PullRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+pullColumns+` WHERE p.repository_id = $1 AND p.number = $2`, repositoryID, number)
	if err != nil {
		return PullRow{}, fmt.Errorf("store: find pull request: %w", err)
	}
	p, err := pgx.CollectExactlyOneRow(rows, scanPull)
	if errors.Is(err, pgx.ErrNoRows) {
		return PullRow{}, ErrNotFound
	}
	if err != nil {
		return PullRow{}, fmt.Errorf("store: find pull request: %w", err)
	}
	return p, nil
}

// ReviewRow is one reviews row with what its usage rows charged it.
type ReviewRow struct {
	ID                string
	PullRequestID     string
	Repository        string
	Number            int
	Title             string
	Status            ReviewStatus
	Trigger           string
	Mode              configfile.ReviewMode
	Scope             review.Scope
	ScopeReason       string
	Model             string
	HeadSHA           string
	MergeBaseSHA      string
	PatchID           string
	PriorReviewID     *string
	SkipReason        repoconfig.SkipReason
	Error             string
	CreatedAt         time.Time
	FinishedAt        *time.Time
	CancelRequestedAt *time.Time
	// Summary is the reviews.summary JSON, nil when the review has none.
	Summary      *review.Summary
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
}

const reviewColumns = `v.id, v.pull_request_id, r.name, p.number, p.title, v.status, v.trigger, v.mode, v.scope, v.scope_reason,
	v.model, v.head_sha, v.merge_base_sha, v.patch_id, v.prior_review_id, v.skip_reason, v.error, v.created_at, v.finished_at,
	v.cancel_requested_at, v.summary, coalesce(u.cost, 0), coalesce(u.input, 0), coalesce(u.output, 0)
	FROM reviews v JOIN pull_requests p ON p.id = v.pull_request_id JOIN repositories r ON r.id = p.repository_id
	LEFT JOIN LATERAL (SELECT sum(cost_usd)::float8 AS cost, sum(input_tokens) AS input, sum(output_tokens) AS output
		FROM usage WHERE review_id = v.id) u ON true`

func scanReview(row pgx.CollectableRow) (ReviewRow, error) {
	var v ReviewRow
	var status, mode, scope, skip string
	var summary []byte
	if err := row.Scan(&v.ID, &v.PullRequestID, &v.Repository, &v.Number, &v.Title, &status, &v.Trigger, &mode, &scope, &v.ScopeReason,
		&v.Model, &v.HeadSHA, &v.MergeBaseSHA, &v.PatchID, &v.PriorReviewID, &skip, &v.Error, &v.CreatedAt, &v.FinishedAt,
		&v.CancelRequestedAt, &summary, &v.CostUSD, &v.InputTokens, &v.OutputTokens); err != nil {
		return v, err
	}
	v.Status, v.Mode = ReviewStatus(status), configfile.ReviewMode(mode)
	v.Scope, v.SkipReason = review.Scope(scope), repoconfig.SkipReason(skip)
	if len(summary) > 0 && string(summary) != "null" {
		var s review.Summary
		if err := json.Unmarshal(summary, &s); err != nil {
			return v, fmt.Errorf("decode summary: %w", err)
		}
		v.Summary = &s
	}
	return v, nil
}

// ListPullReviews returns every review of a pull request, newest first.
func ListPullReviews(ctx context.Context, tx pgx.Tx, pullRequestID string) ([]ReviewRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+reviewColumns+` WHERE v.pull_request_id = $1 ORDER BY v.created_at DESC, v.id DESC`, pullRequestID)
	if err != nil {
		return nil, fmt.Errorf("store: list reviews: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanReview)
	if err != nil {
		return nil, fmt.Errorf("store: list reviews: %w", err)
	}
	return out, nil
}

// FindReview returns one review, or ErrNotFound.
func FindReview(ctx context.Context, tx pgx.Tx, id string) (ReviewRow, error) {
	if uuid.Validate(id) != nil {
		return ReviewRow{}, ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT `+reviewColumns+` WHERE v.id = $1::uuid`, id)
	if err != nil {
		return ReviewRow{}, fmt.Errorf("store: find review: %w", err)
	}
	v, err := pgx.CollectExactlyOneRow(rows, scanReview)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRow{}, ErrNotFound
	}
	if err != nil {
		return ReviewRow{}, fmt.Errorf("store: find review: %w", err)
	}
	return v, nil
}

// FollowupRow is one followups row.
type FollowupRow struct {
	ID             string
	PullRequestID  string
	Repository     string
	Number         int
	CommentID      int64
	Author         string
	Inline         bool
	Path           string
	Line           int
	Status         FollowupStatus
	Reason         string
	ReplyCommentID *int64
	Model          string
	CreatedAt      time.Time
}

// FollowupFilter narrows ListFollowups; zero fields match everything.
type FollowupFilter struct {
	RepositoryID  string
	PullRequestID string
	CommentID     int64
}

// ListFollowups returns a page of follow-ups, newest first.
func ListFollowups(ctx context.Context, tx pgx.Tx, f FollowupFilter, p Page) ([]FollowupRow, *Cursor, error) {
	if err := p.check(); err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT f.id, f.pull_request_id, r.name, p.number, f.comment_id, f.author, f.inline, f.path, f.line,
		f.status, f.reason, f.reply_comment_id, f.model, f.created_at
		FROM followups f JOIN pull_requests p ON p.id = f.pull_request_id JOIN repositories r ON r.id = p.repository_id
		WHERE ($1::uuid IS NULL OR p.repository_id = $1) AND ($2::uuid IS NULL OR f.pull_request_id = $2)
			AND ($3 = 0 OR f.comment_id = $3) AND ($4 OR (f.created_at, f.id) < ($5, $6::uuid))
		ORDER BY f.created_at DESC, f.id DESC LIMIT $7`,
		uuidParam(f.RepositoryID), uuidParam(f.PullRequestID), f.CommentID, p.After.First(), p.After.T, p.afterID(), p.Limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list follow-ups: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (FollowupRow, error) {
		var x FollowupRow
		var status string
		err := row.Scan(&x.ID, &x.PullRequestID, &x.Repository, &x.Number, &x.CommentID, &x.Author, &x.Inline, &x.Path, &x.Line,
			&status, &x.Reason, &x.ReplyCommentID, &x.Model, &x.CreatedAt)
		x.Status = FollowupStatus(status)
		return x, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: list follow-ups: %w", err)
	}
	items, next := paged(out, p.Limit, func(x FollowupRow) Cursor { return Cursor{T: x.CreatedAt, ID: x.ID} })
	return items, next, nil
}
