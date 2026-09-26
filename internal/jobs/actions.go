// actions.go holds the enqueue helpers the web dashboard's API handlers call,
// each running inside the caller's tenant transaction (store.WithTenant) so
// the web role can audit an action in the same transaction it takes effect
// in. Row-level security on pull_requests/reviews already scopes every query
// here to the transaction's tenant; the explicit tenant_id predicates below
// are defense in depth, matching the rest of the codebase.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// ErrNoHead is returned by EnqueueRerun when the pull request has no open
// head to re-review: it is closed, or the number does not exist.
var ErrNoHead = errors.New("jobs: pull request has no head to re-review")

// ErrNotCancelable is returned by RequestCancel when the review is not in a
// state a cancel can reach: it has already finished, it never recorded the
// River job it started as, or reviewID/by is not a well-formed UUID.
var ErrNotCancelable = errors.New("jobs: review is not in a cancelable state")

// ErrRerunQueued is returned by EnqueueRerun when the pull request's
// current head is already being reviewed, or a review job for it is queued:
// a second run would pay the model twice and publish its findings twice.
var ErrRerunQueued = errors.New("jobs: a review of this head is already queued or running")

// ErrRepositoryNotFound is returned by EnqueueReindex when repositoryID does
// not exist in tenantID.
var ErrRepositoryNotFound = errors.New("jobs: repository not found")

// ErrReindexQueued is returned by EnqueueReindex when a forced reindex of
// the repository is already queued or running; no new job was inserted.
var ErrReindexQueued = errors.New("jobs: reindex already queued")

// EnqueueRerun re-queues a review of number's current head, the way a human
// asks kritik to look again. It gives the job a fresh, random Request value
// so it inserts even when a review of the same head already completed,
// bypassing the push-triggered dedup that keys on
// tenant+repository+number+head alone; while a review of that head is
// running or prepared, or a review job for it has yet to finish, it is
// ErrRerunQueued instead.
func EnqueueRerun(
	ctx context.Context, tx pgx.Tx, c *river.Client[pgx.Tx], tenantID, repositoryID string, number int,
) (int64, error) {
	var headSHA string
	err := tx.QueryRow(ctx, `SELECT head_sha FROM pull_requests
		WHERE tenant_id = $1 AND repository_id = $2 AND number = $3 AND state = 'open'`,
		tenantID, repositoryID, number).Scan(&headSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoHead
	}
	if err != nil {
		return 0, fmt.Errorf("jobs: look up pull request head: %w", err)
	}
	// Two re-runs of one pull request at once would each see the other's
	// job not yet committed; the lock makes the second wait and see it.
	key := "kritik:rerun:" + tenantID + ":" + repositoryID + ":" + strconv.Itoa(number)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return 0, fmt.Errorf("jobs: lock pull request: %w", err)
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM reviews r JOIN pull_requests p ON p.id = r.pull_request_id
			WHERE p.tenant_id = $1::text::uuid AND p.repository_id = $2::text::uuid AND p.number = $3 AND r.head_sha = $4
				AND r.status IN ('running', 'prepared'))
		OR EXISTS (
			SELECT 1 FROM river_job
			WHERE kind = $5 AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
				AND args->>'tenant_id' = $1::text AND args->>'repository_id' = $2::text AND (args->>'number')::int = $3
				AND args->>'head_sha' = $4)`,
		tenantID, repositoryID, number, headSHA, ReviewArgs{}.Kind()).Scan(&busy)
	if err != nil {
		return 0, fmt.Errorf("jobs: look up running reviews: %w", err)
	}
	if busy {
		return 0, ErrRerunQueued
	}
	res, err := c.InsertTx(ctx, tx, ReviewArgs{
		TenantID: tenantID, RepositoryID: repositoryID, Number: number, HeadSHA: headSHA,
		Trigger: TriggerManual, Request: uuid.NewString(),
	}, nil)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue rerun: %w", err)
	}
	return res.Job.ID, nil
}

// RequestCancel asks the worker running reviewID to stop. It only applies
// when the review is still running or queued (prepared) and recorded the
// River job it started as, and that job has not ended; otherwise there is
// nothing a cancel can reach.
// The row update and the JobCancelTx call share tx, so a rollback undoes
// both together.
func RequestCancel(ctx context.Context, tx pgx.Tx, c *river.Client[pgx.Tx], reviewID string, by string) error {
	if _, err := uuid.Parse(reviewID); err != nil {
		return ErrNotCancelable
	}
	if _, err := uuid.Parse(by); err != nil {
		return ErrNotCancelable
	}
	var jobID int64
	err := tx.QueryRow(ctx, `UPDATE reviews SET cancel_requested_at = now(), canceled_by = $2
		WHERE id = $1 AND status IN ('running', 'prepared') AND river_job_id IS NOT NULL
		RETURNING river_job_id`, reviewID, by).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotCancelable
	}
	if err != nil {
		return fmt.Errorf("jobs: request cancel: %w", err)
	}
	// A job that already ended can no longer end its review: JobCancelTx
	// would return it unchanged, and nothing would ever act on the request.
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM river_job WHERE id = $1`, jobID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotCancelable
	}
	if err != nil {
		return fmt.Errorf("jobs: read job state: %w", err)
	}
	switch rivertype.JobState(state) {
	case rivertype.JobStateCancelled, rivertype.JobStateCompleted, rivertype.JobStateDiscarded:
		return ErrNotCancelable
	}
	if _, err := c.JobCancelTx(ctx, tx, jobID); err != nil {
		if errors.Is(err, river.ErrNotFound) {
			return ErrNotCancelable
		}
		return fmt.Errorf("jobs: cancel job: %w", err)
	}
	return nil
}

// EnqueueReindex forces a full reindex of a repository even when an active
// generation already covers its current commit. Returns ErrRepositoryNotFound
// if repositoryID does not exist in tenantID, and ErrReindexQueued if a
// forced reindex of the repository is already queued or running: an
// onboarding or push job does not stand in for one.
func EnqueueReindex(ctx context.Context, tx pgx.Tx, c *river.Client[pgx.Tx], tenantID, repositoryID string) (int64, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE tenant_id = $1 AND id = $2)`,
		tenantID, repositoryID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("jobs: look up repository: %w", err)
	}
	if !exists {
		return 0, ErrRepositoryNotFound
	}
	res, err := c.InsertTx(ctx, tx, IndexArgs{
		TenantID: tenantID, RepositoryID: repositoryID, CommitSHA: "", Trigger: TriggerReindex, Full: true,
	}, nil)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue reindex: %w", err)
	}
	if res.UniqueSkippedAsDuplicate {
		return 0, ErrReindexQueued
	}
	return res.Job.ID, nil
}
