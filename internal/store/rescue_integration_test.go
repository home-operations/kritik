//go:build integration

package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"
)

// insertRiverJob puts a running job into River's table as a worker that
// claimed it ago would have left it, and removes it when the test ends.
func insertRiverJob(t *testing.T, ctx context.Context, s *Store, kind, state string, attempt, maxAttempts int, ago time.Duration, metadata string) int64 {
	t.Helper()
	var id int64
	if err := s.owner.QueryRow(ctx, `INSERT INTO river_job (kind, queue, args, state, attempt, max_attempts, attempted_at, metadata, finalized_at)
		VALUES ($1, $1, '{}', $2::text::river_job_state, $3, $4, now() - make_interval(secs => $5), $6::jsonb,
			CASE WHEN $2 IN ('completed', 'discarded', 'cancelled') THEN now() END) RETURNING id`,
		kind, state, attempt, maxAttempts, ago.Seconds(), metadata).Scan(&id); err != nil {
		t.Fatalf("insert river job: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.owner.Exec(context.Background(), `DELETE FROM job_heartbeats WHERE job_id = $1`, id)
		_, _ = s.owner.Exec(context.Background(), `DELETE FROM river_job WHERE id = $1`, id)
	})
	return id
}

// insertRun creates a runner run of job for a review, or for an index
// generation when review is "", and returns the run's and the parent's id.
func insertRun(t *testing.T, ctx context.Context, s *Store, account, review string, job int64) (runID, parentID string) {
	t.Helper()
	err := s.WithAccount(ctx, account, func(tx pgx.Tx) error {
		parentID = review
		kind := RunnerKindReview
		if review == "" {
			kind = RunnerKindIndex
			if err := tx.QueryRow(ctx, `INSERT INTO index_runs (account_id, repository_id, commit_sha, embed_model, embed_dims, mode, status)
				SELECT $1, id, 'abc', 'm', 8, 'full', 'running' FROM repositories WHERE account_id = $1 LIMIT 1 RETURNING id`, account).
				Scan(&parentID); err != nil {
				return err
			}
		}
		var err error
		runID, err = InsertRunnerRun(ctx, tx, account, kind, parentID, job)
		return err
	})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return runID, parentID
}

// rescueFixture is the set of running jobs TestAbandonedJobs and
// TestRescueJob share: one that stopped beating, one still beating, one
// claimed too recently to judge, one on its last attempt, one a cancel was
// asked of, and one that completed.
type rescueFixture struct {
	silent, beating, fresh, lastTry, cancelled, done int64
	alpha, review                                    string
}

const rescueStale = 3 * time.Minute

func newRescueFixture(t *testing.T, ctx context.Context, s *Store) rescueFixture {
	t.Helper()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	f := rescueFixture{
		alpha:     accountID(t, s, "alpha"),
		silent:    insertRiverJob(t, ctx, s, "review", "running", 1, 25, 10*time.Minute, `{}`),
		beating:   insertRiverJob(t, ctx, s, "review", "running", 1, 25, 10*time.Minute, `{}`),
		fresh:     insertRiverJob(t, ctx, s, "review", "running", 1, 25, time.Minute, `{}`),
		lastTry:   insertRiverJob(t, ctx, s, "index", "running", 3, 3, 10*time.Minute, `{}`),
		cancelled: insertRiverJob(t, ctx, s, "followup", "running", 1, 25, 10*time.Minute, `{"cancel_attempted_at": "2026-01-01T00:00:00Z"}`),
		done:      insertRiverJob(t, ctx, s, "review", "completed", 1, 25, 10*time.Minute, `{}`),
	}
	for _, id := range []int64{f.beating, f.fresh, f.done} {
		if err := s.HeartbeatJob(ctx, id); err != nil {
			t.Fatalf("HeartbeatJob(%d): %v", id, err)
		}
	}
	f.review = insertReview(t, ctx, s, f.alpha)
	if err := s.WithAccount(ctx, f.alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET river_job_id = $2 WHERE id = $1`, f.review, f.silent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// abandoned lists the fixture's abandoned jobs, in order.
func (f rescueFixture) abandoned(t *testing.T, ctx context.Context, s *Store) []AbandonedJob {
	t.Helper()
	got, err := s.AbandonedJobs(ctx, rescueStale, 100)
	if err != nil {
		t.Fatalf("AbandonedJobs: %v", err)
	}
	ids := make([]int64, 0, len(got))
	for _, j := range got {
		ids = append(ids, j.ID)
	}
	if want := []int64{f.silent, f.lastTry, f.cancelled}; !slices.Equal(ids, want) {
		t.Fatalf("abandoned = %v, want %v: no heartbeat for stale, claimed longer ago than that, and still running", ids, want)
	}
	return got
}

// jobRow is what a rescue leaves on River's row.
type jobRow struct {
	state     string
	finalized bool
	soon      bool
	errs      []string
}

func readJob(t *testing.T, ctx context.Context, s *Store, id int64) jobRow {
	t.Helper()
	var r jobRow
	if err := s.owner.QueryRow(ctx, `SELECT state::text, finalized_at IS NOT NULL, scheduled_at > now() - interval '10 seconds',
		coalesce((SELECT array_agg(e->>'error') FROM unnest(errors) e), '{}') FROM river_job WHERE id = $1`, id).
		Scan(&r.state, &r.finalized, &r.soon, &r.errs); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAbandonedJobs(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := newRescueFixture(t, ctx, s)
	got := f.abandoned(t, ctx, s)
	if j := got[0]; j.Kind != "review" || j.Attempt != 1 || j.MaxAttempts != 25 || j.CancelRequested || j.RescueState() != rivertype.JobStateRetryable {
		t.Fatalf("silent job = %+v", j)
	}
	if j := got[1]; j.RescueState() != rivertype.JobStateDiscarded {
		t.Fatalf("last-try job = %+v, want discarded", j)
	}
	if j := got[2]; !j.CancelRequested || j.RescueState() != rivertype.JobStateCancelled {
		t.Fatalf("cancelled job = %+v, want cancelled", j)
	}
}

func TestRescueJob(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	f := newRescueFixture(t, ctx, s)
	got := f.abandoned(t, ctx, s)

	// A beat since the listing keeps the job: its worker is alive after all.
	if err := s.HeartbeatJob(ctx, f.silent); err != nil {
		t.Fatal(err)
	}
	if rescued, err := s.RescueJob(ctx, got[0], rescueStale, "lost"); err != nil || rescued {
		t.Fatalf("RescueJob after a beat = %v, %v; want left alone", rescued, err)
	}
	if _, err := s.owner.Exec(ctx, `UPDATE job_heartbeats SET heartbeat_at = now() - interval '10 minutes' WHERE job_id = $1`, f.silent); err != nil {
		t.Fatal(err)
	}
	for _, j := range got {
		if rescued, err := s.RescueJob(ctx, j, rescueStale, "lost"); err != nil || !rescued {
			t.Fatalf("RescueJob(%d) = %v, %v; want rescued", j.ID, rescued, err)
		}
		if rescued, err := s.RescueJob(ctx, j, rescueStale, "lost"); err != nil || rescued {
			t.Fatalf("second RescueJob(%d) = %v, %v; want nothing left to rescue", j.ID, rescued, err)
		}
	}
	if r := readJob(t, ctx, s, f.silent); r.state != "retryable" || r.finalized || !r.soon || !slices.Equal(r.errs, []string{"lost"}) {
		t.Fatalf("silent job after rescue = %+v, want retryable now with the reason recorded", r)
	}
	if r := readJob(t, ctx, s, f.lastTry); r.state != "discarded" || !r.finalized || len(r.errs) != 1 {
		t.Fatalf("last-try job after rescue = %+v, want discarded", r)
	}
	if r := readJob(t, ctx, s, f.cancelled); r.state != "cancelled" || !r.finalized {
		t.Fatalf("cancelled job after rescue = %+v, want cancelled", r)
	}
	for _, id := range []int64{f.beating, f.fresh, f.done} {
		if r := readJob(t, ctx, s, id); len(r.errs) != 0 {
			t.Fatalf("job %d was touched: %+v", id, r)
		}
	}
	var status, errText string
	if err := s.WithAccount(ctx, f.alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, error FROM reviews WHERE id = $1`, f.review).Scan(&status, &errText)
	}); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || errText != "lost" {
		t.Fatalf("the rescued job's review = %s %q, want failed with the reason", status, errText)
	}

	// The beats of jobs no longer running go; a running job keeps its own.
	if n, err := s.SweepJobHeartbeats(ctx); err != nil || n != 2 {
		t.Fatalf("SweepJobHeartbeats = %d, %v; want the rescued and the completed job's beats swept", n, err)
	}
	rows, err := s.owner.Query(ctx, `SELECT job_id FROM job_heartbeats WHERE job_id = ANY($1) ORDER BY job_id`, []int64{f.silent, f.beating, f.fresh, f.done})
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{f.beating, f.fresh}; !slices.Equal(left, want) {
		t.Fatalf("heartbeats left = %v, want %v", left, want)
	}
}

func TestOrphanedRuns(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if err := s.ApplyConfig(ctx, parse(t, twoAccounts)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	alpha := accountID(t, s, "alpha")
	live := insertRiverJob(t, ctx, s, "review", "running", 1, 25, time.Minute, `{}`)
	retried := insertRiverJob(t, ctx, s, "review", "retryable", 1, 25, 10*time.Minute, `{}`)
	gone := insertRiverJob(t, ctx, s, "index", "discarded", 3, 3, 10*time.Minute, `{}`)
	liveRun, _ := insertRun(t, ctx, s, alpha, insertReview(t, ctx, s, alpha), live)
	reviewRun, review := insertRun(t, ctx, s, alpha, insertReview(t, ctx, s, alpha), retried)
	indexRun, indexGen := insertRun(t, ctx, s, alpha, "", gone)
	// A run River never knew the job of, from before runs recorded one.
	var legacy string
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO runner_runs (account_id, kind) VALUES ($1, 'index') RETURNING id`, alpha).Scan(&legacy)
	}); err != nil {
		t.Fatal(err)
	}
	// A run of the retried job that its worker did end.
	endedRun, _ := insertRun(t, ctx, s, alpha, insertReview(t, ctx, s, alpha), retried)
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runner_runs SET finished_at = now() WHERE id = $1`, endedRun)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.OrphanedRuns(ctx, 100)
	if err != nil {
		t.Fatalf("OrphanedRuns: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if want := []string{reviewRun, indexRun}; !slices.Equal(ids, want) {
		t.Fatalf("orphaned = %v, want %v: unfinished, of a job not running (not %s, %s or %s)", ids, want, liveRun, legacy, endedRun)
	}
	if r := got[0]; r.AccountID != alpha || r.Kind != RunnerKindReview || r.ReviewID != review || r.IndexRunID != "" {
		t.Fatalf("review run = %+v", r)
	}
	if r := got[1]; r.Kind != RunnerKindIndex || r.IndexRunID != indexGen || r.ReviewID != "" {
		t.Fatalf("index run = %+v", r)
	}
	for range 2 { // idempotent
		for _, r := range got {
			if err := s.EndOrphanedRun(ctx, r, "lost"); err != nil {
				t.Fatalf("EndOrphanedRun(%s): %v", r.ID, err)
			}
		}
	}
	var phase, runErr, reviewStatus, indexStatus string
	var finished bool
	if err := s.WithAccount(ctx, alpha, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT phase, error, finished_at IS NOT NULL FROM runner_runs WHERE id = $1`, reviewRun).
			Scan(&phase, &runErr, &finished); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM reviews WHERE id = $1`, review).Scan(&reviewStatus); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM index_runs WHERE id = $1`, indexGen).Scan(&indexStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if phase != "failed" || runErr != "lost" || !finished || reviewStatus != "failed" || indexStatus != "failed" {
		t.Fatalf("after ending: run %s %q finished=%v, review %s, index run %s; want all failed", phase, runErr, finished, reviewStatus, indexStatus)
	}
	if left, err := s.OrphanedRuns(ctx, 100); err != nil || len(left) != 0 {
		t.Fatalf("OrphanedRuns after ending = %v, %v; want none", left, err)
	}
}
