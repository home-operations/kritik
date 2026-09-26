package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// UsageGroup is what a usage series is keyed by.
type UsageGroup string

// Usage series groupings.
const (
	UsageByDay   UsageGroup = "day"
	UsageByModel UsageGroup = "model"
	UsageByRepo  UsageGroup = "repo"
	UsageByRole  UsageGroup = "role"
)

// Valid reports whether g is a usage grouping.
func (g UsageGroup) Valid() bool {
	return g == UsageByDay || g == UsageByModel || g == UsageByRepo || g == UsageByRole
}

// usageKeys are the SQL key expressions of each grouping: over usage u
// joined to its repository ur, and over model_calls m joined through its
// review to the repository mr. A model call's kind maps to the usage role
// it is charged as.
var usageKeys = map[UsageGroup][2]string{
	UsageByDay:   {`to_char(u.created_at, 'YYYY-MM-DD')`, `to_char(m.created_at, 'YYYY-MM-DD')`},
	UsageByModel: {`u.model`, `m.model`},
	UsageByRepo:  {`coalesce(ur.name, '')`, `coalesce(mr.name, '')`},
	UsageByRole:  {`u.role`, `CASE m.kind WHEN 'agent_step' THEN 'review' ELSE m.kind END`},
}

// UsageSeriesRow is one key of a usage series. Tokens, cost and calls come
// from the usage rows, the durable accounting; the cache token counts come
// from the model calls, which only the transcript retention keeps, so they
// cover at most that window.
type UsageSeriesRow struct {
	Key              string
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	CostUSD          float64
	Calls            int64
}

// UsageSeries sums the tenant's usage in [from, to) by group, ordered by
// key.
func UsageSeries(ctx context.Context, tx pgx.Tx, group UsageGroup, from, to time.Time) ([]UsageSeriesRow, error) {
	keys, ok := usageKeys[group]
	if !ok {
		return nil, ErrFilter
	}
	rows, err := tx.Query(ctx, `WITH billed AS (
			SELECT `+keys[0]+` AS key, sum(u.input_tokens) AS input, sum(u.output_tokens) AS output,
				sum(u.cost_usd)::float8 AS cost, count(*) AS calls
			FROM usage u LEFT JOIN repositories ur ON ur.id = u.repository_id
			WHERE u.created_at >= $1 AND u.created_at < $2 GROUP BY 1),
		cached AS (
			SELECT `+keys[1]+` AS key, sum(m.cache_read_tokens) AS cache_read, sum(m.cache_write_tokens) AS cache_write
			FROM model_calls m LEFT JOIN reviews mv ON mv.id = m.review_id
				LEFT JOIN pull_requests mp ON mp.id = mv.pull_request_id LEFT JOIN repositories mr ON mr.id = mp.repository_id
			WHERE m.created_at >= $1 AND m.created_at < $2 GROUP BY 1)
		SELECT coalesce(b.key, c.key), coalesce(b.input, 0), coalesce(c.cache_read, 0), coalesce(c.cache_write, 0),
			coalesce(b.output, 0), coalesce(b.cost, 0), coalesce(b.calls, 0)
		FROM billed b FULL JOIN cached c ON c.key = b.key ORDER BY 1`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: usage series: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageSeriesRow, error) {
		var u UsageSeriesRow
		err := row.Scan(&u.Key, &u.InputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.OutputTokens, &u.CostUSD, &u.Calls)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: usage series: %w", err)
	}
	return out, nil
}

// JobState is a river_job.state value.
type JobState string

// River job states.
const (
	JobAvailable JobState = "available"
	JobScheduled JobState = "scheduled"
	JobRunning   JobState = "running"
	JobRetryable JobState = "retryable"
	JobPending   JobState = "pending"
	JobCompleted JobState = "completed"
	JobCancelled JobState = "cancelled"
	JobDiscarded JobState = "discarded"
)

// JobRow is one River job of the tenant, with the parts of its arguments
// the queue view shows.
type JobRow struct {
	ID           int64
	Kind         string
	State        JobState
	Attempt      int
	MaxAttempts  int
	CreatedAt    time.Time
	ScheduledAt  time.Time
	AttemptedAt  *time.Time
	FinalizedAt  *time.Time
	RepositoryID string
	Repository   string
	Number       int
	Head         string
	Trigger      string
	CommentID    int64
	LastError    string
}

// queueFinished bounds how many finished jobs the queue view lists;
// queueActive bounds the unfinished ones, which a healthy queue keeps far
// below it.
const (
	queueFinished = 50
	queueActive   = 500
)

// ListQueue returns the tenant's review, follow-up and index jobs that
// have not finished, oldest first, then the most recently finished ones.
// River's tables carry no row-level security, so the tenant comes from
// the transaction's own setting rather than a parameter a caller could get
// wrong.
func ListQueue(ctx context.Context, tx pgx.Tx) ([]JobRow, error) {
	const cols = `j.id, j.kind, j.state::text, j.attempt, j.max_attempts, j.created_at, j.scheduled_at, j.attempted_at, j.finalized_at,
		coalesce(j.args->>'repository_id', ''), coalesce(r.name, ''), coalesce((j.args->>'number')::int, 0),
		coalesce(j.args->>'head_sha', j.args->>'commit_sha', ''), coalesce(j.args->>'trigger', ''),
		coalesce((j.args->>'comment_id')::bigint, 0), coalesce(j.errors[array_length(j.errors, 1)]->>'error', '')
		FROM river_job j LEFT JOIN repositories r ON r.id::text = j.args->>'repository_id'
		WHERE j.args->>'tenant_id' = current_setting('app.tenant_id', true) AND j.kind IN ('review', 'followup', 'index')`
	rows, err := tx.Query(ctx, `(SELECT `+cols+` AND j.state IN ('available', 'scheduled', 'running', 'retryable')
			ORDER BY j.scheduled_at, j.id LIMIT $1)
		UNION ALL
		(SELECT `+cols+` AND j.state IN ('completed', 'cancelled', 'discarded')
			ORDER BY j.finalized_at DESC NULLS LAST, j.id DESC LIMIT $2)`, queueActive, queueFinished)
	if err != nil {
		return nil, fmt.Errorf("store: list queue: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (JobRow, error) {
		var j JobRow
		var state string
		err := row.Scan(&j.ID, &j.Kind, &state, &j.Attempt, &j.MaxAttempts, &j.CreatedAt, &j.ScheduledAt, &j.AttemptedAt, &j.FinalizedAt,
			&j.RepositoryID, &j.Repository, &j.Number, &j.Head, &j.Trigger, &j.CommentID, &j.LastError)
		j.State = JobState(state)
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list queue: %w", err)
	}
	return out, nil
}
