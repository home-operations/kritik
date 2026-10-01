package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"
)

// HeartbeatJob stamps the worker's heartbeat on the River job it is
// working, so the leader knows the job's replica is alive.
func (s *Store) HeartbeatJob(ctx context.Context, jobID int64) error {
	if _, err := s.app.Exec(ctx, `INSERT INTO job_heartbeats (job_id, heartbeat_at) VALUES ($1, now())
		ON CONFLICT (job_id) DO UPDATE SET heartbeat_at = now()`, jobID); err != nil {
		return fmt.Errorf("store: heartbeat job %d: %w", jobID, err)
	}
	return nil
}

// AbandonedJob is a River job in the running state whose worker has not
// stamped a heartbeat for longer than the stale window: its replica died
// without handing the job back.
type AbandonedJob struct {
	ID                   int64
	Kind                 string
	Attempt, MaxAttempts int
	// CancelRequested says a cancel was asked of the job while it ran.
	CancelRequested bool
}

// RescueState is the state the job is handed back in, as River's own
// rescuer would: cancelled when a cancel was asked of it, retryable while
// it has attempts left, discarded otherwise.
func (j AbandonedJob) RescueState() rivertype.JobState {
	switch {
	case j.CancelRequested:
		return rivertype.JobStateCancelled
	case j.Attempt < max(j.MaxAttempts, 0):
		return rivertype.JobStateRetryable
	default:
		return rivertype.JobStateDiscarded
	}
}

// AbandonedJobs lists up to limit running jobs whose worker has stamped
// no heartbeat for stale, oldest first. A job claimed less than stale ago
// is left alone: its first beat may be on its way. Owner connection: River's
// rows are outside row-level security and the leader spans every account.
func (s *Store) AbandonedJobs(ctx context.Context, stale time.Duration, limit int) ([]AbandonedJob, error) {
	if s.owner == nil {
		return nil, errors.New("store: AbandonedJobs needs the owner connection")
	}
	rows, err := s.owner.Query(ctx, `SELECT j.id, j.kind, j.attempt, j.max_attempts, j.metadata ? 'cancel_attempted_at'
		FROM river_job j LEFT JOIN job_heartbeats h ON h.job_id = j.id
		WHERE j.state = 'running' AND j.attempted_at < now() - make_interval(secs => $1)
		  AND (h.heartbeat_at IS NULL OR h.heartbeat_at < now() - make_interval(secs => $1))
		ORDER BY j.id LIMIT $2`, stale.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list abandoned jobs: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AbandonedJob, error) {
		var j AbandonedJob
		err := row.Scan(&j.ID, &j.Kind, &j.Attempt, &j.MaxAttempts, &j.CancelRequested)
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list abandoned jobs: %w", err)
	}
	return jobs, nil
}

// RescueJob hands job back to River in its RescueState, with reason as
// the attempt's error, and ends the reviews the job left running, which
// would otherwise block every re-run of their head. It reports whether it
// did: false when the job finished, was claimed again or stamped a
// heartbeat within stale since it was listed. Owner connection.
func (s *Store) RescueJob(ctx context.Context, job AbandonedJob, stale time.Duration, reason string) (bool, error) {
	if s.owner == nil {
		return false, errors.New("store: RescueJob needs the owner connection")
	}
	attemptErr, err := json.Marshal(rivertype.AttemptError{At: time.Now().UTC(), Attempt: max(job.Attempt, 0), Error: reason})
	if err != nil {
		return false, fmt.Errorf("store: encode attempt error: %w", err)
	}
	state := string(job.RescueState())
	tx, err := s.owner.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	tag, err := tx.Exec(ctx, `UPDATE river_job j SET state = $3::text::river_job_state,
		scheduled_at = CASE WHEN $3::text = 'retryable' THEN now() ELSE j.scheduled_at END,
		finalized_at = CASE WHEN $3::text = 'retryable' THEN NULL ELSE now() END,
		errors = array_append(j.errors, $4::jsonb)
		WHERE j.id = $1 AND j.state = 'running' AND j.attempt = $2
		  AND NOT EXISTS (SELECT 1 FROM job_heartbeats h WHERE h.job_id = j.id AND h.heartbeat_at >= now() - make_interval(secs => $5))`,
		job.ID, job.Attempt, state, string(attemptErr), stale.Seconds())
	if err != nil {
		return false, fmt.Errorf("store: rescue job %d: %w", job.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE reviews SET status = 'failed', error = left($2, 2000), finished_at = now()
		WHERE river_job_id = $1 AND finished_at IS NULL`, job.ID, reason); err != nil {
		return false, fmt.Errorf("store: end the reviews of job %d: %w", job.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit: %w", err)
	}
	return true, nil
}

// OrphanedRun is a runner run that never ended although the River job that
// started it is no longer running: its worker died, and its Kubernetes Job,
// if one was created, runs on with nobody watching it.
type OrphanedRun struct {
	ID, AccountID string
	Kind          RunnerKind
	// ReviewID or IndexRunID is the run's parent, the other "".
	ReviewID, IndexRunID string
}

// OrphanedRuns lists up to limit unfinished runs whose River job is not
// running, oldest first. A run's job is running for as long as the worker
// is inside Work, which is where the run is ended; a run still open after
// that was left by a worker that died, or whose record of it failed. Owner
// connection.
func (s *Store) OrphanedRuns(ctx context.Context, limit int) ([]OrphanedRun, error) {
	if s.owner == nil {
		return nil, errors.New("store: OrphanedRuns needs the owner connection")
	}
	rows, err := s.owner.Query(ctx, `SELECT r.id, r.account_id, r.kind, coalesce(r.review_id::text, ''), coalesce(r.index_run_id::text, '')
		FROM runner_runs r
		WHERE r.finished_at IS NULL AND r.river_job_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM river_job j WHERE j.id = r.river_job_id AND j.state = 'running')
		ORDER BY r.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list orphaned runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OrphanedRun, error) {
		var r OrphanedRun
		err := row.Scan(&r.ID, &r.AccountID, &r.Kind, &r.ReviewID, &r.IndexRunID)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list orphaned runs: %w", err)
	}
	return runs, nil
}

// EndOrphanedRun ends run failed for reason, and its parent review or
// index run with it unless something ended that already. Owner connection.
func (s *Store) EndOrphanedRun(ctx context.Context, run OrphanedRun, reason string) error {
	if s.owner == nil {
		return errors.New("store: EndOrphanedRun needs the owner connection")
	}
	tx, err := s.owner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, `UPDATE runner_runs SET phase = 'failed', error = left($2, 2000), finished_at = now()
		WHERE id = $1 AND finished_at IS NULL`, run.ID, reason); err != nil {
		return fmt.Errorf("store: end orphaned run %s: %w", run.ID, err)
	}
	switch {
	case run.ReviewID != "":
		if _, err := tx.Exec(ctx, `UPDATE reviews SET status = 'failed', error = left($2, 2000), finished_at = now()
			WHERE id = $1 AND finished_at IS NULL`, run.ReviewID, reason); err != nil {
			return fmt.Errorf("store: end the review of run %s: %w", run.ID, err)
		}
	case run.IndexRunID != "":
		if _, err := tx.Exec(ctx, `UPDATE index_runs SET status = 'failed', error = left($2, 2000), finished_at = now()
			WHERE id = $1 AND finished_at IS NULL`, run.IndexRunID, reason); err != nil {
			return fmt.Errorf("store: end the index run of run %s: %w", run.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// SweepJobHeartbeats deletes the heartbeats of jobs no longer running, and
// returns how many. A worker leaves its last beat behind when its job ends,
// since River marks the job only after Work returns, so the rows are
// cleared here rather than racing that. Owner connection.
func (s *Store) SweepJobHeartbeats(ctx context.Context) (int64, error) {
	if s.owner == nil {
		return 0, errors.New("store: SweepJobHeartbeats needs the owner connection")
	}
	tag, err := s.owner.Exec(ctx, `DELETE FROM job_heartbeats h
		WHERE NOT EXISTS (SELECT 1 FROM river_job j WHERE j.id = h.job_id AND j.state = 'running')`)
	if err != nil {
		return 0, fmt.Errorf("store: sweep job heartbeats: %w", err)
	}
	return tag.RowsAffected(), nil
}
