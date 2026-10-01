package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AnalyticsGroup is the width of one bucket of an analytics series.
type AnalyticsGroup string

// Analytics groupings.
const (
	AnalyticsByDay   AnalyticsGroup = "day"
	AnalyticsByWeek  AnalyticsGroup = "week"
	AnalyticsByMonth AnalyticsGroup = "month"
)

// Valid reports whether g is an analytics grouping.
func (g AnalyticsGroup) Valid() bool {
	return g == AnalyticsByDay || g == AnalyticsByWeek || g == AnalyticsByMonth
}

// AnalyticsTotals is what an account's reviews came to over a window.
// Reviews and pull requests count completed reviews; findings count each
// pull request's finding once, in the window it was first reported,
// Addressed how many of those a later review no longer reported, and the
// reactions the ones posted inline drew.
type AnalyticsTotals struct {
	PullRequests  int
	Reviews       int
	Failed        int
	Findings      SeverityCounts
	Addressed     int
	ReactionsUp   int
	ReactionsDown int
	CostUSD       float64
	// MedianReviewMs is nil when no review completed, and MedianMergeMs,
	// from opened to merged, when no pull request kritika knows merged.
	MedianReviewMs *int64
	MedianMergeMs  *int64
}

// ReadAnalyticsTotals sums the account's reviews in [from, to).
func ReadAnalyticsTotals(ctx context.Context, tx pgx.Tx, from, to time.Time) (AnalyticsTotals, error) {
	var t AnalyticsTotals
	var median *float64
	err := tx.QueryRow(ctx, `SELECT count(DISTINCT pull_request_id) FILTER (WHERE status = 'completed'),
			count(*) FILTER (WHERE status = 'completed'), count(*) FILTER (WHERE status = 'failed'),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at - created_at) * 1000)
				FILTER (WHERE status = 'completed' AND finished_at IS NOT NULL)
		FROM reviews WHERE created_at >= $1 AND created_at < $2`, from, to).
		Scan(&t.PullRequests, &t.Reviews, &t.Failed, &median)
	if err != nil {
		return t, fmt.Errorf("store: analytics totals: %w", err)
	}
	if median != nil {
		ms := int64(*median)
		t.MedianReviewMs = &ms
	}
	err = tx.QueryRow(ctx, `WITH `+findingIssues+`
		SELECT count(*) FILTER (WHERE severity = 'blocking'), count(*) FILTER (WHERE severity = 'important'),
			count(*) FILTER (WHERE severity = 'nit'), count(*) FILTER (WHERE addressed),
			coalesce(sum(reactions_up), 0), coalesce(sum(reactions_down), 0)
		FROM latest WHERE first_at >= $1 AND first_at < $2`, from, to).
		Scan(&t.Findings.Blocking, &t.Findings.Important, &t.Findings.Nit, &t.Addressed, &t.ReactionsUp, &t.ReactionsDown)
	if err != nil {
		return t, fmt.Errorf("store: analytics findings: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(cost_usd), 0)::float8 FROM usage WHERE created_at >= $1 AND created_at < $2`, from, to).
		Scan(&t.CostUSD)
	if err != nil {
		return t, fmt.Errorf("store: analytics spend: %w", err)
	}
	var merge *float64
	err = tx.QueryRow(ctx, `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM closed_at - opened_at) * 1000)
		FROM pull_requests WHERE merged AND opened_at IS NOT NULL AND closed_at >= $1 AND closed_at < $2`, from, to).Scan(&merge)
	if err != nil {
		return t, fmt.Errorf("store: analytics merge time: %w", err)
	}
	if merge != nil {
		ms := int64(*merge)
		t.MedianMergeMs = &ms
	}
	return t, nil
}

// AnalyticsPoint is one bucket of an analytics series, keyed by the date
// it starts on.
type AnalyticsPoint struct {
	Key      string
	Reviews  int
	Findings SeverityCounts
	CostUSD  float64
}

// ReadAnalyticsSeries buckets the account's completed reviews, findings (by
// when each was first reported) and spend in [from, to) by group, every
// bucket present, the first starting where from falls.
func ReadAnalyticsSeries(ctx context.Context, tx pgx.Tx, group AnalyticsGroup, from, to time.Time) ([]AnalyticsPoint, error) {
	if !group.Valid() {
		return nil, ErrFilter
	}
	rows, err := tx.Query(ctx, `WITH `+findingIssues+`,
		buckets AS (
			SELECT b FROM generate_series(date_trunc($3, $1::timestamptz), $2::timestamptz - interval '1 microsecond',
				('1 ' || $3)::interval) AS b),
		rev AS (
			SELECT date_trunc($3, created_at) AS b, count(*) AS n FROM reviews
			WHERE status = 'completed' AND created_at >= $1 AND created_at < $2 GROUP BY 1),
		fnd AS (
			SELECT date_trunc($3, first_at) AS b, count(*) FILTER (WHERE severity = 'blocking') AS blocking,
				count(*) FILTER (WHERE severity = 'important') AS important, count(*) FILTER (WHERE severity = 'nit') AS nit
			FROM latest WHERE first_at >= $1 AND first_at < $2 GROUP BY 1),
		spend AS (
			SELECT date_trunc($3, created_at) AS b, sum(cost_usd)::float8 AS cost FROM usage
			WHERE created_at >= $1 AND created_at < $2 GROUP BY 1)
		SELECT to_char(k.b, 'YYYY-MM-DD'), coalesce(rev.n, 0), coalesce(fnd.blocking, 0), coalesce(fnd.important, 0),
			coalesce(fnd.nit, 0), coalesce(spend.cost, 0)
		FROM buckets k LEFT JOIN rev ON rev.b = k.b LEFT JOIN fnd ON fnd.b = k.b LEFT JOIN spend ON spend.b = k.b
		ORDER BY k.b`, from, to, string(group))
	if err != nil {
		return nil, fmt.Errorf("store: analytics series: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AnalyticsPoint, error) {
		var p AnalyticsPoint
		err := row.Scan(&p.Key, &p.Reviews, &p.Findings.Blocking, &p.Findings.Important, &p.Findings.Nit, &p.CostUSD)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: analytics series: %w", err)
	}
	return out, nil
}

// RepoActivity is what one repository's reviews came to over a window.
type RepoActivity struct {
	Repository string
	Reviews    int
	Findings   SeverityCounts
	Addressed  int
}

// ReadRepoActivity lists the repositories with completed reviews or
// findings in [from, to), most reviewed first, at most limit of them.
func ReadRepoActivity(ctx context.Context, tx pgx.Tx, from, to time.Time, limit int) ([]RepoActivity, error) {
	rows, err := tx.Query(ctx, `WITH `+findingIssues+`,
		rev AS (
			SELECT p.repository_id AS id, count(*) AS n FROM reviews v JOIN pull_requests p ON p.id = v.pull_request_id
			WHERE v.status = 'completed' AND v.created_at >= $1 AND v.created_at < $2 GROUP BY 1),
		fnd AS (
			SELECT p.repository_id AS id, count(*) FILTER (WHERE l.severity = 'blocking') AS blocking,
				count(*) FILTER (WHERE l.severity = 'important') AS important, count(*) FILTER (WHERE l.severity = 'nit') AS nit,
				count(*) FILTER (WHERE l.addressed) AS addressed
			FROM latest l JOIN pull_requests p ON p.id = l.pull_request_id
			WHERE l.first_at >= $1 AND l.first_at < $2 GROUP BY 1)
		SELECT r.name, coalesce(rev.n, 0), coalesce(fnd.blocking, 0), coalesce(fnd.important, 0), coalesce(fnd.nit, 0),
			coalesce(fnd.addressed, 0)
		FROM repositories r LEFT JOIN rev ON rev.id = r.id LEFT JOIN fnd ON fnd.id = r.id
		WHERE rev.id IS NOT NULL OR fnd.id IS NOT NULL
		ORDER BY coalesce(rev.n, 0) DESC, r.name LIMIT $3`, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("store: repository activity: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RepoActivity, error) {
		var a RepoActivity
		err := row.Scan(&a.Repository, &a.Reviews, &a.Findings.Blocking, &a.Findings.Important, &a.Findings.Nit, &a.Addressed)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: repository activity: %w", err)
	}
	return out, nil
}
